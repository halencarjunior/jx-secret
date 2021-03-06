package populate

import (
	"fmt"
	"path/filepath"
	"time"

	"github.com/jenkins-x/jx-helpers/pkg/cmdrunner"
	"github.com/jenkins-x/jx-helpers/pkg/cobras/helper"
	"github.com/jenkins-x/jx-helpers/pkg/cobras/templates"
	"github.com/jenkins-x/jx-helpers/pkg/termcolor"
	"github.com/jenkins-x/jx-logging/pkg/log"
	"github.com/jenkins-x/jx-secret/pkg/apis/schema/v1alpha1"
	"github.com/jenkins-x/jx-secret/pkg/cmd/vault/wait"
	"github.com/jenkins-x/jx-secret/pkg/extsecrets/editor"
	"github.com/jenkins-x/jx-secret/pkg/extsecrets/editor/factory"
	"github.com/jenkins-x/jx-secret/pkg/extsecrets/secretfacade"
	"github.com/jenkins-x/jx-secret/pkg/rootcmd"
	"github.com/jenkins-x/jx-secret/pkg/schema"
	"github.com/jenkins-x/jx-secret/pkg/schema/generators"
	"github.com/pkg/errors"
	"github.com/spf13/cobra"
)

var (
	cmdLong = templates.LongDesc(`
		Populates any missing secret values which can be automatically generated"
`)

	cmdExample = templates.Examples(`
		%s populate
	`)
)

// Options the options for the command
type Options struct {
	secretfacade.Options

	Dir           string
	WaitDuration  time.Duration
	Schema        *v1alpha1.Schema
	Results       []*secretfacade.SecretPair
	CommandRunner cmdrunner.CommandRunner
	NoWait        bool
	Generators    map[string]generators.Generator
}

// NewCmdPopulate creates a command object for the command
func NewCmdPopulate() (*cobra.Command, *Options) {
	o := &Options{}

	cmd := &cobra.Command{
		Use:     "populate",
		Short:   "Populates any missing secret values which can be automatically generated",
		Long:    cmdLong,
		Example: fmt.Sprintf(cmdExample, rootcmd.BinaryName),
		Run: func(cmd *cobra.Command, args []string) {
			err := o.Run()
			helper.CheckErr(err)
		},
	}
	cmd.Flags().StringVarP(&o.Namespace, "ns", "n", "", "the namespace to filter the ExternalSecret resources")
	cmd.Flags().StringVarP(&o.Dir, "dir", "d", ".", "the directory to look for the .jx/gitops/secret-schema.yaml file")
	cmd.Flags().BoolVarP(&o.NoWait, "no-wait", "", false, "disables waiting for the secret store (e.g. vault) to be available")
	cmd.Flags().DurationVarP(&o.WaitDuration, "wait", "w", 5*time.Minute, "the maximum time period to wait for the vault pod to be ready if using the vault backendType")
	return cmd, o
}

// Run implements the command
func (o *Options) Run() error {
	// get a list of external secrets which do not have corresponding k8s secret data populated
	results, err := o.Verify()
	if err != nil {
		return errors.Wrap(err, "failed to verify secrets")
	}
	o.Results = results

	if len(results) == 0 {
		log.Logger().Infof("the %d ExternalSecrets are %s", len(o.ExternalSecrets), termcolor.ColorInfo("populated"))
		return nil
	}
	o.loadGenerators()

	editors := map[string]editor.Interface{}
	waited := map[string]bool{}

	o.Schema, err = schema.LoadSchema(filepath.Join(o.Dir, ".jx", "gitops", "secret-schema.yaml"))
	if err != nil {
		return errors.Wrapf(err, "failed to load survey schema used to prompt the user for questions")
	}
	for _, r := range results {
		name := r.ExternalSecret.Name
		backendType := r.ExternalSecret.Spec.BackendType

		// lets wait until the backend is available
		if !waited[backendType] {
			err = o.waitForBackend(backendType)
			if err != nil {
				return errors.Wrapf(err, "failed to wait for backend type %s", backendType)
			}
			waited[backendType] = true
		}

		secEditor := editors[backendType]
		log.Logger().Infof("using %s as the secrets store", backendType)
		if secEditor == nil {
			secEditor, err = factory.NewEditor(&r.ExternalSecret, o.CommandRunner, o.KubeClient)
			if err != nil {
				return errors.Wrapf(err, "failed to create a secret editor for ExternalSecret %s", name)
			}
			editors[backendType] = secEditor
		}

		if r.Error != nil {
			for _, e := range r.Error.EntryErrors {
				keyProperties := &editor.KeyProperties{
					Key: e.Key,
				}
				for _, property := range e.Properties {
					var value string
					value, err = o.generateSecretValue(name, property, e)
					if err != nil {
						return errors.Wrapf(err, "failed to ask user secret value property %s for key %s on ExternalSecret %s", property, e.Key, name)
					}
					if value == "" {
						continue
					}
					keyProperties.Properties = append(keyProperties.Properties, editor.PropertyValue{
						Property: property,
						Value:    value,
					})
				}

				if len(keyProperties.Properties) > 0 {
					err = secEditor.Write(keyProperties)
					if err != nil {
						return errors.Wrapf(err, "failed to save properties %s on ExternalSecret %s", keyProperties.String(), name)
					}
				}
			}
		}
	}
	return nil
}

func (o *Options) generateSecretValue(secretName, property string, e *secretfacade.EntryError) (string, error) {
	object, propertySchema, err := schema.FindObjectProperty(o.Schema, secretName, property)
	if err != nil {
		return "", errors.Wrapf(err, "failed to find schema for entry %s property %s", e.Key, property)
	}
	if propertySchema == nil {
		return "", nil
	}

	if propertySchema.DefaultValue != "" {
		return propertySchema.DefaultValue, nil
	}

	generatorName := propertySchema.Generator
	if generatorName == "" {
		return "", nil
	}

	generator := o.Generators[generatorName]
	if generator == nil {
		return "", errors.Errorf("could not find generator %s for property %s in object %s", generatorName, property, secretName)
	}

	args := &generators.Arguments{
		Schema:   o.Schema,
		Object:   object,
		Property: propertySchema,
	}
	value, err := generator(args)
	if err != nil {
		return value, errors.Wrapf(err, "failed to invoke generator %s for property %s in object %s", generatorName, property, secretName)
	}
	return value, nil
}

func (o *Options) waitForBackend(backendType string) error {
	if backendType != "vault" {
		return nil
	}
	if o.NoWait {
		log.Logger().Infof("disabling waiting for vault pod to be ready")
		return nil
	}

	_, wo := wait.NewCmdWait()
	wo.WaitDuration = o.WaitDuration
	wo.KubeClient = o.KubeClient

	err := wo.Run()
	if err != nil {
		return errors.Wrapf(err, "failed to wait for vault backend")
	}
	return nil
}

func (o *Options) loadGenerators() {
	if o.Generators == nil {
		o.Generators = map[string]generators.Generator{}
	}
	o.Generators["hmac"] = generators.Hmac
	o.Generators["password"] = generators.Password
	o.Generators["gitOperator.username"] = generators.SecretEntry(o.KubeClient, o.Namespace, "jx-boot", "username")
	o.Generators["gitOperator.password"] = generators.SecretEntry(o.KubeClient, o.Namespace, "jx-boot", "password")
}
