package edit

import (
	"fmt"
	"path/filepath"
	"strings"

	v1 "github.com/jenkins-x/jx-secret/pkg/apis/external/v1"
	schemaapi "github.com/jenkins-x/jx-secret/pkg/apis/schema/v1alpha1"
	"github.com/jenkins-x/jx-secret/pkg/schema"

	"github.com/jenkins-x/jx-helpers/pkg/cmdrunner"
	"github.com/jenkins-x/jx-helpers/pkg/cobras/helper"
	"github.com/jenkins-x/jx-helpers/pkg/cobras/templates"
	"github.com/jenkins-x/jx-helpers/pkg/input"
	"github.com/jenkins-x/jx-helpers/pkg/input/survey"
	"github.com/jenkins-x/jx-helpers/pkg/termcolor"
	"github.com/jenkins-x/jx-logging/pkg/log"
	"github.com/jenkins-x/jx-secret/pkg/extsecrets/editor"
	"github.com/jenkins-x/jx-secret/pkg/extsecrets/editor/factory"
	"github.com/jenkins-x/jx-secret/pkg/extsecrets/secretfacade"
	"github.com/jenkins-x/jx-secret/pkg/rootcmd"
	"github.com/pkg/errors"
	"github.com/spf13/cobra"
)

var (
	editLong = templates.LongDesc(`
		Edits any missing properties in the ExternalSecret resources
`)

	editExample = templates.Examples(`
		%s edit
	`)
)

// Options the options for the command
type Options struct {
	secretfacade.Options

	Dir           string
	Filter        string
	Input         input.Interface
	Schema        *schemaapi.Schema
	Results       []*secretfacade.SecretPair
	CommandRunner cmdrunner.CommandRunner
}

// NewCmdEdit creates a command object for the command
func NewCmdEdit() (*cobra.Command, *Options) {
	o := &Options{}

	cmd := &cobra.Command{
		Use:     "edit",
		Short:   "Edits any missing properties in the ExternalSecret resources",
		Long:    editLong,
		Example: fmt.Sprintf(editExample, rootcmd.BinaryName),
		Run: func(cmd *cobra.Command, args []string) {
			err := o.Run()
			helper.CheckErr(err)
		},
	}
	cmd.Flags().StringVarP(&o.Namespace, "ns", "n", "", "the namespace to filter the ExternalSecret resources")
	cmd.Flags().StringVarP(&o.Dir, "dir", "d", ".", "the directory to look for the .jx/gitops/secret-schema.yaml file")
	cmd.Flags().StringVarP(&o.Filter, "filter", "f", "", "filter on the Secret / ExternalSecret names to enter")
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

	if o.Input == nil {
		o.Input = survey.NewInput()
	}

	editors := map[string]editor.Interface{}

	o.Schema, err = schema.LoadSchema(filepath.Join(o.Dir, ".jx", "gitops", "secret-schema.yaml"))
	if err != nil {
		return errors.Wrapf(err, "failed to load survey schema used to prompt the user for questions")
	}
	for _, r := range results {
		name := r.ExternalSecret.Name
		backendType := r.ExternalSecret.Spec.BackendType
		secEditor := editors[backendType]
		log.Logger().Infof("using %s as the secrets store", backendType)
		if secEditor == nil {
			secEditor, err = factory.NewEditor(&r.ExternalSecret, o.CommandRunner, o.KubeClient)
			if err != nil {
				return errors.Wrapf(err, "failed to create a secret editor for ExternalSecret %s", name)
			}
			editors[backendType] = secEditor
		}

		// todo do we need to find any surveys that require a confirm?
		// order them somehow?
		// maybe skip any?
		if o.Matches(r) {
			data := o.DataToEdit(r)

			m := map[string]*editor.KeyProperties{}
			for i := range data {
				d := &data[i]
				key := d.Key
				property := d.Property
				keyProperties := m[key]
				if keyProperties == nil {
					keyProperties = &editor.KeyProperties{
						Key: key,
					}
					m[key] = keyProperties
				}

				var value string
				value, err = o.askForSecretValue(r, d)
				if err != nil {
					return errors.Wrapf(err, "failed to ask user secret value property %s for key %s on ExternalSecret %s", property, key, name)
				}

				keyProperties.Properties = append(keyProperties.Properties, editor.PropertyValue{
					Property: property,
					Value:    value,
				})

			}
			for _, keyProperties := range m {
				err = secEditor.Write(keyProperties)
				if err != nil {
					return errors.Wrapf(err, "failed to save properties %s on ExternalSecret %s", keyProperties.String(), name)
				}
			}
		}
	}
	return nil
}

func (o *Options) askForSecretValue(s *secretfacade.SecretPair, d *v1.Data) (string, error) {
	var value string
	var err error
	var propertySpec *schemaapi.Property
	name := s.ExternalSecret.Name
	property := d.Property
	_, propertySpec, err = schema.FindObjectProperty(o.Schema, name, property)
	if err != nil {
		return "", errors.Wrapf(err, "failed to find schema property for object %s property %s", name, property)
	}
	if propertySpec == nil {
		message, help := o.propertyMessage(s, d)
		value, err = o.Input.PickPassword(message, help) //nolint:govet
		if err != nil {
			return "", errors.Wrapf(err, "failed to enter property %s for key %s on ExternalSecret %s", property, d.Key, name)
		}
		return value, nil
	}

	// if mask

	// if format

	// if pattern?

	// min / max

	// if confirm

	// if git get the kind URL / template the help and question?

	// Add TESTS!!!

	kind := propertySpec.Labels[schemaapi.LabelKind]
	switch kind {
	case "confirm":
		log.Logger().Warn("implement confirm")
	default:
		return o.Input.PickPassword(propertySpec.Question, propertySpec.Help) //nolint:govet
	}
	return value, nil
}

func (o *Options) propertyMessage(s *secretfacade.SecretPair, d *v1.Data) (string, string) {
	name := s.ExternalSecret.Name
	property := d.Property
	return name + "." + property, ""
}

// Matches returns true if the secret matches the current filter
// If no filter then just filter out mandatory properties only?
func (o *Options) Matches(r *secretfacade.SecretPair) bool {
	if o.Filter == "" {
		return r.IsInvalid()
	}
	return strings.Contains(r.ExternalSecret.Name, o.Filter)
}

// DataToEdit returns the properties to edit
func (o *Options) DataToEdit(r *secretfacade.SecretPair) []v1.Data {
	// if filtering return all properties
	if o.Filter != "" {
		return r.ExternalSecret.Spec.Data
	}

	missingProperties := map[string]bool{}
	if r.Error != nil {
		for _, e := range r.Error.EntryErrors {
			for _, n := range e.Properties {
				missingProperties[n] = true
			}
		}
	}

	// otherwise return only missing fields
	var results []v1.Data
	for _, d := range r.ExternalSecret.Spec.Data {
		if missingProperties[d.Property] {
			results = append(results, d)
		}
	}
	return results
}
