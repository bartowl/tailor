package openshift

import (
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/ghodss/yaml"
	"github.com/opendevstack/tailor/cli"
	"github.com/opendevstack/tailor/utils"
	"github.com/xeipuuv/gojsonpointer"
)

var (
	tailorOriginalValuesAnnotationPrefix = "original-values.tailor.io"
	tailorManagedAnnotation              = "managed-annotations.tailor.opendevstack.org"
	platformManagedFields                = []string{
		"/metadata/generation",
		"/metadata/creationTimestamp",
		"/spec/tags",
		"/status",
		"/spec/volumeName",
		"/spec/template/metadata/creationTimestamp",
	}
	emptyMapFields = []string{
		"/metadata/annotations",
		"/spec/template/metadata/annotations",
	}
	immutableFields = map[string][]string{
		"Route": []string{
			"/spec/host",
		},
		"PersistentVolumeClaim": []string{
			"/spec/accessModes",
			"/spec/storageClassName",
			"/spec/resources/requests/storage",
		},
	}
	platformModifiedFields = []string{
		"/spec/template/spec/containers/[0-9]+/image$",
	}
)

type ResourceItem struct {
	Source                   string
	Kind                     string
	Name                     string
	Labels                   map[string]interface{}
	Annotations              map[string]interface{}
	Paths                    []string
	Config                   map[string]interface{}
	TailorManagedAnnotations []string
}

func NewResourceItem(m map[string]interface{}, source string) (*ResourceItem, error) {
	item := &ResourceItem{Source: source}
	err := item.parseConfig(m)
	return item, err
}

func (i *ResourceItem) FullName() string {
	return i.Kind + "/" + i.Name
}

func (templateItem *ResourceItem) ChangesFrom(platformItem *ResourceItem, externallyModifiedPaths []string) ([]*Change, error) {
	err := templateItem.prepareForComparisonWithPlatformItem(platformItem, externallyModifiedPaths)
	if err != nil {
		return nil, err
	}
	err = platformItem.prepareForComparisonWithTemplateItem(templateItem)
	if err != nil {
		return nil, err
	}

	comparison := map[string]*JsonPatch{}
	addedPaths := []string{}

	for _, path := range templateItem.Paths {
		// Skip subpaths of already added paths
		if utils.IncludesPrefix(addedPaths, path) {
			continue
		}

		pathPointer, _ := gojsonpointer.NewJsonPointer(path)
		templateItemVal, _, _ := pathPointer.Get(templateItem.Config)
		platformItemVal, _, err := pathPointer.Get(platformItem.Config)

		if err != nil {
			// Pointer does not exist in platformItem
			if templateItem.isImmutableField(path) {
				return recreateChanges(templateItem, platformItem), nil
			} else {
				comparison[path] = &JsonPatch{Op: "add", Value: templateItemVal}
				addedPaths = append(addedPaths, path)
			}
		} else {
			// Pointer exists in both items
			switch templateItemVal.(type) {
			case []interface{}:
				// slice content changed, continue ...
				comparison[path] = &JsonPatch{Op: "noop"}
			case []string:
				// slice content changed, continue ...
				comparison[path] = &JsonPatch{Op: "noop"}
			case map[string]interface{}:
				// map content changed, continue
				comparison[path] = &JsonPatch{Op: "noop"}
			default:
				if templateItemVal == platformItemVal {
					comparison[path] = &JsonPatch{Op: "noop"}
				} else {
					if templateItem.isImmutableField(path) {
						return recreateChanges(templateItem, platformItem), nil
					} else {
						comparison[path] = &JsonPatch{Op: "replace", Value: templateItemVal}
					}
				}
			}
		}
	}

	deletedPaths := []string{}

	for _, path := range platformItem.Paths {
		if _, ok := comparison[path]; !ok {
			// Do not delete subpaths of already deleted paths
			if utils.IncludesPrefix(deletedPaths, path) {
				continue
			}
			// Pointer exist only in platformItem
			comparison[path] = &JsonPatch{Op: "remove"}
			deletedPaths = append(deletedPaths, path)
		}
	}

	c := &Change{
		Kind:         templateItem.Kind,
		Name:         templateItem.Name,
		Patches:      []*JsonPatch{},
		CurrentState: platformItem.YamlConfig(),
		DesiredState: templateItem.YamlConfig(),
	}

	for path, patch := range comparison {
		if patch.Op != "noop" {
			patch.Path = path
			c.AddPatch(patch)
		}
	}

	if len(c.Patches) > 0 {
		c.Action = "Update"
	} else {
		c.Action = "Noop"
	}

	return []*Change{c}, nil
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

	// Add empty maps
	for _, p := range emptyMapFields {
		initPointer, _ := gojsonpointer.NewJsonPointer(p)
		_, _, err := initPointer.Get(m)
		if err != nil {
			initPointer.Set(m, make(map[string]interface{}))
		}
	}

	// Extract annotations
	annotationsPointer, _ := gojsonpointer.NewJsonPointer("/metadata/annotations")
	annotations, _, err := annotationsPointer.Get(m)
	i.Annotations = make(map[string]interface{})
	if err == nil {
		for k, v := range annotations.(map[string]interface{}) {
			i.Annotations[k] = v
		}
	}

	// Figure out which annotations are managed by Tailor
	i.TailorManagedAnnotations = []string{}
	if i.Source == "platform" {
		// For platform items, only annotation listed in tailorManagedAnnotation are managed
		p, _ := gojsonpointer.NewJsonPointer("/metadata/annotations/" + tailorManagedAnnotation)
		managedAnnotations, _, err := p.Get(m)
		if err == nil {
			i.TailorManagedAnnotations = strings.Split(managedAnnotations.(string), ",")
		}
	} else { // source = template
		// For template items, all annotations are managed
		for k, _ := range i.Annotations {
			i.TailorManagedAnnotations = append(i.TailorManagedAnnotations, k)
		}
		// If there are any managed annotations, we need to set tailorManagedAnnotation
		if len(i.TailorManagedAnnotations) > 0 {
			p, _ := gojsonpointer.NewJsonPointer("/metadata/annotations/" + tailorManagedAnnotation)
			sort.Strings(i.TailorManagedAnnotations)
			p.Set(m, strings.Join(i.TailorManagedAnnotations, ","))
		}
	}

	// Remove platform-managed fields
	for _, p := range platformManagedFields {
		deletePointer, _ := gojsonpointer.NewJsonPointer(p)
		_, _ = deletePointer.Delete(m)
	}

	i.Config = m

	// Build list of JSON pointers
	i.walkMap(m, "")

	// Handle platform-modified fields:
	// If there is an annotation, copy its value into the spec, otherwise
	// copy the spec value into the annotation.
	newPaths := []string{}
	for _, path := range i.Paths {
		for _, platformModifiedField := range platformModifiedFields {
			matched, _ := regexp.MatchString(platformModifiedField, path)
			if matched {
				annotationKey := strings.Replace(strings.TrimLeft(path, "/"), "/", ".", -1)
				annotationPath := "/metadata/annotations/" + tailorOriginalValuesAnnotationPrefix + "~1" + annotationKey
				annotationPointer, _ := gojsonpointer.NewJsonPointer(annotationPath)
				specPointer, _ := gojsonpointer.NewJsonPointer(path)
				specValue, _, _ := specPointer.Get(i.Config)
				annotationValue, _, err := annotationPointer.Get(i.Config)
				if err == nil {
					cli.DebugMsg("Platform: Setting", path, "to", annotationValue.(string))
					_, err := specPointer.Set(i.Config, annotationValue)
					if err != nil {
						return err
					}
				} else {
					// Ensure there is an annotation map before setting values in it
					anP, _ := gojsonpointer.NewJsonPointer("/metadata/annotations")
					_, _, err := anP.Get(i.Config)
					if err != nil {
						anP.Set(i.Config, map[string]interface{}{})
						newPaths = append(newPaths, "/metadata/annotations")
					}
					cli.DebugMsg("Template: Setting", annotationPath, "to", specValue.(string))
					_, err = annotationPointer.Set(i.Config, specValue)
					if err != nil {
						return err
					}
					newPaths = append(newPaths, annotationPath)
				}
			}
		}
	}
	if len(newPaths) > 0 {
		i.Paths = append(i.Paths, newPaths...)
	}

	return nil
}
func (i *ResourceItem) RemoveUnmanagedAnnotations() {
	for a := range i.Annotations {
		managed := false
		for _, m := range i.TailorManagedAnnotations {
			if a == m {
				managed = true
			}
		}
		if !managed {
			cli.DebugMsg("Removing unmanaged annotation", a)
			path := "/metadata/annotations/" + utils.JSONPointerPath(a)
			deletePointer, _ := gojsonpointer.NewJsonPointer(path)
			_, err := deletePointer.Delete(i.Config)
			if err != nil {
				cli.DebugMsg("WARN: Could not remove unmanaged annotation", a)
				fmt.Printf("%v", i.Config)
			}
		}
	}
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
		}
		_, err = pathPointer.Set(templateItem.Config, platformItemVal)
		if err != nil {
			cli.DebugMsg(
				"Could not set",
				path,
				"to",
				platformItemVal.(string),
				"in template item",
				templateItem.FullName(),
			)
		}
	}

	return nil
}

// prepareForComparisonWithTemplateItem massages platform item in such a way
// that it can be compared with the given template item:
// - remove all annotations which are not managed
// - massage apiVersion to deal with namespace introduction in 3.11 on cluster
//   if client is still on 3.9
func (platformItem *ResourceItem) prepareForComparisonWithTemplateItem(templateItem *ResourceItem) error {
	unmanagedAnnotations := []string{}
	for a, _ := range platformItem.Annotations {
		if a == tailorManagedAnnotation {
			continue
		}
		if strings.HasPrefix(a, tailorOriginalValuesAnnotationPrefix) {
			continue
		}
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
		cli.DebugMsg("Delete path", path, "from configuration")
		deletePointer, _ := gojsonpointer.NewJsonPointer(path)
		_, err := deletePointer.Delete(platformItem.Config)
		if err != nil {
			return fmt.Errorf("Could not delete %s from configuration", path)
		}
		platformItem.Paths = utils.Remove(platformItem.Paths, path)
	}

	templateAPIVersionPointer, _ := gojsonpointer.NewJsonPointer("/apiVersion")
	templateAPIVersionVal, _, err := templateAPIVersionPointer.Get(templateItem.Config)
	cli.DebugMsg("Got version", templateAPIVersionVal.(string), "in template item")
	if err != nil {
		return nil
	}
	platformAPIVersionPointer, _ := gojsonpointer.NewJsonPointer("/apiVersion")
	platformAPIVersionVal, _, err := platformAPIVersionPointer.Get(platformItem.Config)
	cli.DebugMsg("Got version", platformAPIVersionVal.(string), "in platform item")
	if err != nil {
		return nil
	}
	if strings.HasSuffix(templateAPIVersionVal.(string), "/v1") && platformAPIVersionVal.(string) == "v1" {
		cli.DebugMsg("Setting platform version to", templateAPIVersionVal.(string))
		platformAPIVersionPointer.Set(platformItem.Config, templateAPIVersionVal)
	}

	return nil
}
