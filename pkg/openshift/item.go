package openshift

import (
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/ghodss/yaml"
	"github.com/opendevstack/tailor/pkg/cli"
	"github.com/opendevstack/tailor/pkg/utils"
	"github.com/xeipuuv/gojsonpointer"
)

var (
	annotationsPath                      = "/metadata/annotations"
	tailorAnnotationPrefix               = "tailor.opendevstack.org"
	tailorAppliedConfigAnnotation        = tailorAnnotationPrefix + "/applied-config"
	escapedTailorAppliedConfigAnnotation = strings.Replace(tailorAppliedConfigAnnotation, "/", "~1", -1)
	tailorAppliedConfigAnnotationPath    = annotationsPath + "/" + escapedTailorAppliedConfigAnnotation
	tailorManagedAnnotation              = tailorAnnotationPrefix + "/managed-annotations"
	escapedTailorManagedAnnotation       = strings.Replace(tailorManagedAnnotation, "/", "~1", -1)
	tailorManagedAnnotationPath          = annotationsPath + "/" + escapedTailorManagedAnnotation
	platformManagedSimpleFields          = []string{
		"/metadata/generation",
		"/metadata/creationTimestamp",
		"/spec/tags",
		"/status",
		"/spec/volumeName",
		"/spec/template/metadata/creationTimestamp",
	}
	platformManagedRegexFields = []string{
		"^/spec/triggers/[0-9]*/imageChangeParams/lastTriggeredImage",
	}
	immutableFields = map[string][]string{
		"PersistentVolumeClaim": []string{
			"/spec/accessModes",
			"/spec/storageClassName",
			"/spec/resources/requests/storage",
		},
		"Route": []string{
			"/spec/host",
		},
		"Secret": []string{
			"/type",
		},
	}
	platformModifiedFields = []string{
		"/spec/template/spec/containers/[0-9]+/image$",
	}

	KindMapping = map[string]string{
		"svc":                   "Service",
		"service":               "Service",
		"route":                 "Route",
		"dc":                    "DeploymentConfig",
		"deploymentconfig":      "DeploymentConfig",
		"bc":                    "BuildConfig",
		"buildconfig":           "BuildConfig",
		"is":                    "ImageStream",
		"imagestream":           "ImageStream",
		"pvc":                   "PersistentVolumeClaim",
		"persistentvolumeclaim": "PersistentVolumeClaim",
		"template":              "Template",
		"cm":                    "ConfigMap",
		"configmap":             "ConfigMap",
		"secret":                "Secret",
		"rolebinding":           "RoleBinding",
		"serviceaccount":        "ServiceAccount",
	}
)

type ResourceItem struct {
	Source                    string
	Kind                      string
	Name                      string
	Labels                    map[string]interface{}
	Annotations               map[string]interface{}
	Paths                     []string
	Config                    map[string]interface{}
	TailorManagedAnnotations  []string
	TailorAppliedConfigFields map[string]string
	AnnotationsPresent        bool
}

func NewResourceItem(m map[string]interface{}, source string) (*ResourceItem, error) {
	item := &ResourceItem{Source: source}
	err := item.parseConfig(m)
	return item, err
}

func (i *ResourceItem) FullName() string {
	return i.Kind + "/" + i.Name
}

func (i *ResourceItem) HasLabel(label string) bool {
	labelParts := strings.Split(label, "=")
	if _, ok := i.Labels[labelParts[0]]; !ok {
		return false
	} else if i.Labels[labelParts[0]].(string) != labelParts[1] {
		return false
	}
	return true
}

func (i *ResourceItem) YamlConfig() string {
	y, _ := yaml.Marshal(i.Config)
	return string(y)
}

// parseConfig uses the config to initialise an item. The logic is the same
// for template and platform items, with no knowledge of the "other" item - it
// may or may not exist.
func (i *ResourceItem) parseConfig(m map[string]interface{}) error {
	// Extract kind and name
	kindPointer, _ := gojsonpointer.NewJsonPointer("/kind")
	kind, _, err := kindPointer.Get(m)
	if err != nil {
		return err
	}
	i.Kind = kind.(string)
	namePointer, _ := gojsonpointer.NewJsonPointer("/metadata/name")
	name, _, err := namePointer.Get(m)
	if err != nil {
		return err
	}
	i.Name = name.(string)

	// Extract labels
	labelsPointer, _ := gojsonpointer.NewJsonPointer("/metadata/labels")
	labels, _, err := labelsPointer.Get(m)
	if err != nil {
		i.Labels = make(map[string]interface{})
	} else {
		i.Labels = labels.(map[string]interface{})
	}

	// Extract annotations
	annotationsPointer, _ := gojsonpointer.NewJsonPointer("/metadata/annotations")
	annotations, _, err := annotationsPointer.Get(m)
	i.Annotations = make(map[string]interface{})
	i.AnnotationsPresent = false
	if err == nil {
		i.AnnotationsPresent = true
		for k, v := range annotations.(map[string]interface{}) {
			i.Annotations[k] = v
		}
	}

	// Figure out which annotations are managed by Tailor
	i.TailorManagedAnnotations = []string{}
	if i.Source == "platform" {
		// For platform items, only annotation listed in tailorManagedAnnotation are managed
		p, err := gojsonpointer.NewJsonPointer(tailorManagedAnnotationPath)
		if err != nil {
			return fmt.Errorf("Could not create JSON pointer %s: %s", tailorManagedAnnotationPath, err)
		}
		managedAnnotations, _, err := p.Get(m)
		if err == nil {
			i.TailorManagedAnnotations = strings.Split(managedAnnotations.(string), ",")
			_, err = p.Delete(m)
			if err != nil {
				return fmt.Errorf("Could not delete %s: %s", tailorManagedAnnotationPath, err)
			}
			delete(i.Annotations, tailorManagedAnnotation)
		}
	} else { // source = template
		// For template items, all annotations are managed
		for k := range i.Annotations {
			i.TailorManagedAnnotations = append(i.TailorManagedAnnotations, k)
		}
		sort.Strings(i.TailorManagedAnnotations)
	}

	// Applied configuration
	// Unfortunately the configuration we apply is sometimes overwritten with
	// actual values. To be still able to compare, we need to store the applied
	// configuration as an annotation.
	i.TailorAppliedConfigFields = map[string]string{}
	// If source is platform, we copy the values in the annotation into the
	// corresponding spec locations.
	if i.Source == "platform" {
		annotationPointer, err := gojsonpointer.NewJsonPointer(tailorAppliedConfigAnnotationPath)
		if err != nil {
			return fmt.Errorf("Could not create JSON pointer %s: %s", tailorAppliedConfigAnnotationPath, err)
		}
		val, _, err := annotationPointer.Get(m)
		if err == nil {
			valBytes := []byte(val.(string))
			v := map[string]string{}
			err = json.Unmarshal(valBytes, &v)
			i.TailorAppliedConfigFields = v
			if err != nil {
				return fmt.Errorf("Could not unmarshal JSON %s: %s", tailorAppliedConfigAnnotationPath, val)
			}
			for k, v := range i.TailorAppliedConfigFields {
				specPointer, err := gojsonpointer.NewJsonPointer(k)
				if err != nil {
					return fmt.Errorf("Could not create JSON pointer %s: %s", k, err)
				}
				_, err = specPointer.Set(m, v)
				if err != nil {
					return fmt.Errorf("Could not set %s: %s", k, err)
				}
			}
			_, err = annotationPointer.Delete(m)
			if err != nil {
				return fmt.Errorf("Could not delete %s: %s", tailorAppliedConfigAnnotationPath, err)
			}
		}
		delete(i.Annotations, tailorAppliedConfigAnnotation)
	}

	// Remove platform-managed simple fields
	for _, p := range platformManagedSimpleFields {
		deletePointer, _ := gojsonpointer.NewJsonPointer(p)
		_, _ = deletePointer.Delete(m)
	}

	i.Config = m

	// Build list of JSON pointers
	i.walkMap(m, "")

	// Iterate over extracted paths and massage as necessary
	newPaths := []string{}
	deletedPathIndices := []int{}
	for pathIndex, path := range i.Paths {

		// Remove platform-managed regex fields
		for _, platformManagedField := range platformManagedRegexFields {
			matched, _ := regexp.MatchString(platformManagedField, path)
			if matched {
				deletePointer, _ := gojsonpointer.NewJsonPointer(path)
				_, _ = deletePointer.Delete(i.Config)
				deletedPathIndices = append(deletedPathIndices, pathIndex)
			}
		}

		// Applied configuration
		// If source is template, we need to check if the current path
		// needs to be stored in the applied-config annotation.
		if i.Source == "template" {
			for _, platformModifiedField := range platformModifiedFields {
				matched, _ := regexp.MatchString(platformModifiedField, path)
				if matched {
					specPointer, err := gojsonpointer.NewJsonPointer(path)
					if err != nil {
						return fmt.Errorf("Could not create JSON pointer %s: %s", path, err)
					}
					specValue, _, err := specPointer.Get(i.Config)
					if err != nil {
						return fmt.Errorf("Could not get value of %s: %s", path, err)
					}
					i.TailorAppliedConfigFields[path] = specValue.(string)

				}
			}
		}
	}

	// As we delete items from a slice, we need to adjust the pre-calculated
	// indices to delete (shift to left by one for each deletion).
	indexOffset := 0
	for _, pathIndex := range deletedPathIndices {
		deletionIndex := pathIndex + indexOffset
		cli.DebugMsg("Removing platform managed path", i.Paths[deletionIndex])
		i.Paths = append(i.Paths[:deletionIndex], i.Paths[deletionIndex+1:]...)
		indexOffset = indexOffset - 1
	}
	if len(newPaths) > 0 {
		i.Paths = append(i.Paths, newPaths...)
	}

	return nil
}

func (i *ResourceItem) isImmutableField(field string) bool {
	for _, key := range immutableFields[i.Kind] {
		if key == field {
			return true
		}
	}
	return false
}

func (i *ResourceItem) walkMap(m map[string]interface{}, pointer string) {
	for k, v := range m {
		i.handleKeyValue(k, v, pointer)
	}
}

func (i *ResourceItem) walkArray(a []interface{}, pointer string) {
	for k, v := range a {
		i.handleKeyValue(k, v, pointer)
	}
}

func (i *ResourceItem) handleKeyValue(k interface{}, v interface{}, pointer string) {

	strK := ""
	switch kv := k.(type) {
	case string:
		strK = kv
	case int:
		strK = strconv.Itoa(kv)
	}

	relativePointer := utils.JSONPointerPath(strK)
	absolutePointer := pointer + "/" + relativePointer
	i.Paths = append(i.Paths, absolutePointer)

	switch vv := v.(type) {
	case []interface{}:
		i.walkArray(vv, absolutePointer)
	case map[string]interface{}:
		i.walkMap(vv, absolutePointer)
	}
}

func recreateChanges(templateItem, platformItem *ResourceItem) []*Change {
	deleteChange := &Change{
		Action:       "Delete",
		Kind:         templateItem.Kind,
		Name:         templateItem.Name,
		CurrentState: platformItem.YamlConfig(),
		DesiredState: "",
	}
	createChange := &Change{
		Action:       "Create",
		Kind:         templateItem.Kind,
		Name:         templateItem.Name,
		CurrentState: "",
		DesiredState: templateItem.YamlConfig(),
	}
	return []*Change{deleteChange, createChange}
}

// prepareForComparisonWithPlatformItem massages template item in such a way
// that it can be compared with the given platform item:
// - copy value from platformItem to templateItem for externally modified paths
func (templateItem *ResourceItem) prepareForComparisonWithPlatformItem(platformItem *ResourceItem, externallyModifiedPaths []string) error {
	for _, path := range externallyModifiedPaths {
		pathPointer, _ := gojsonpointer.NewJsonPointer(path)
		platformItemVal, _, err := pathPointer.Get(platformItem.Config)
		if err != nil {
			cli.DebugMsg("No such path", path, "in platform item", platformItem.FullName())
		} else {
			_, err = pathPointer.Set(templateItem.Config, platformItemVal)
			if err != nil {
				cli.DebugMsg(fmt.Sprintf(
					"Could not set %s to %v in template item %s",
					path,
					platformItemVal,
					templateItem.FullName(),
				))
			} else {
				// Add ignored path and its subpaths to the paths slice
				// of the template item.
				templateItem.Paths = append(templateItem.Paths, path)
				switch vv := platformItemVal.(type) {
				case []interface{}:
					templateItem.walkArray(vv, path)
				case map[string]interface{}:
					templateItem.walkMap(vv, path)
				}
			}
		}
	}

	return nil
}

// prepareForComparisonWithTemplateItem massages platform item in such a way
// that it can be compared with the given template item:
// - remove all annotations which are not managed
func (platformItem *ResourceItem) prepareForComparisonWithTemplateItem(templateItem *ResourceItem) error {
	unmanagedAnnotations := []string{}
	for a := range platformItem.Annotations {
		if utils.Includes(templateItem.TailorManagedAnnotations, a) {
			continue
		}
		if utils.Includes(platformItem.TailorManagedAnnotations, a) {
			continue
		}
		unmanagedAnnotations = append(unmanagedAnnotations, a)
	}
	for _, a := range unmanagedAnnotations {
		path := "/metadata/annotations/" + utils.JSONPointerPath(a)
		deletePointer, _ := gojsonpointer.NewJsonPointer(path)
		_, err := deletePointer.Delete(platformItem.Config)
		if err != nil {
			return fmt.Errorf("Could not delete %s from configuration", path)
		}
		platformItem.Paths = utils.Remove(platformItem.Paths, path)
	}
	return nil
}