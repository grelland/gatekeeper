package target

import (
	"encoding/json"
	"fmt"

	"github.com/open-policy-agent/frameworks/constraint/pkg/core/constraints"
	"github.com/open-policy-agent/frameworks/constraint/pkg/handler"
	"github.com/open-policy-agent/gatekeeper/v3/pkg/mutation/match"
	"github.com/pkg/errors"
	admissionv1 "k8s.io/api/admission/v1"
	"k8s.io/apiextensions-apiserver/pkg/apis/apiextensions"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/apis/meta/v1/validation"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/validation/field"
)

// nolint: revive // Moved error out of pkg/webhook/admission; needs capitalization for backwards compat.
var ErrOldObjectIsNil = errors.New("oldObject cannot be nil for DELETE operations")

// Name is the name of Gatekeeper's Kubernetes validation target.
const Name = "admission.k8s.gatekeeper.sh"

type K8sValidationTarget struct {
	cache nsCache
}

var (
	_ handler.TargetHandler = &K8sValidationTarget{}
	_ handler.Cacher        = &K8sValidationTarget{}
)

func (h *K8sValidationTarget) GetName() string {
	return Name
}

func (h *K8sValidationTarget) processUnstructured(o *unstructured.Unstructured) (bool, []string, interface{}, error) {
	// Namespace will be "" for cluster objects
	gvk := o.GetObjectKind().GroupVersionKind()
	if gvk.Version == "" {
		return true, nil, nil, fmt.Errorf("%w: resource %s has no version", ErrRequestObject, o.GetName())
	}
	if gvk.Kind == "" {
		return true, nil, nil, fmt.Errorf("%w: resource %s has no kind", ErrRequestObject, o.GetName())
	}

	var path []string
	if o.GetNamespace() == "" {
		path = clusterScopedKey(gvk, o.GetName())
	} else {
		path = namespaceScopedKey(o.GetNamespace(), gvk, o.GetName())
	}

	return true, path, o.Object, nil
}

func clusterScopedKey(gvk schema.GroupVersionKind, name string) []string {
	return []string{"cluster", gvk.GroupVersion().String(), gvk.Kind, name}
}

func namespaceScopedKey(namespace string, gvk schema.GroupVersionKind, name string) []string {
	return []string{"namespace", namespace, gvk.GroupVersion().String(), gvk.Kind, name}
}

func (h *K8sValidationTarget) ProcessData(obj interface{}) (bool, []string, interface{}, error) {
	switch data := obj.(type) {
	case unstructured.Unstructured:
		return h.processUnstructured(&data)
	case *unstructured.Unstructured:
		return h.processUnstructured(data)
	case wipeData, *wipeData:
		return true, nil, nil, nil
	default:
		return false, nil, nil, nil
	}
}

func (h *K8sValidationTarget) HandleReview(obj interface{}) (bool, interface{}, error) {
	return h.handleReview(obj)
}

// handleReview returns a complete *gkReview to pass to the Client.
func (h *K8sValidationTarget) handleReview(obj interface{}) (bool, *gkReview, error) {
	var err error
	var review *gkReview

	switch data := obj.(type) {
	case admissionv1.AdmissionRequest:
		review = &gkReview{AdmissionRequest: data}
	case *admissionv1.AdmissionRequest:
		review = &gkReview{AdmissionRequest: *data}
	case AugmentedReview:
		review = &gkReview{
			AdmissionRequest: *data.AdmissionRequest,
			namespace:        data.Namespace,
			source:           data.Source,
			isAdmission:      data.IsAdmission,
		}
	case *AugmentedReview:
		review = &gkReview{
			AdmissionRequest: *data.AdmissionRequest,
			namespace:        data.Namespace,
			source:           data.Source,
			isAdmission:      data.IsAdmission,
		}
	case AugmentedUnstructured:
		review, err = augmentedUnstructuredToAdmissionRequest(data)
		if err != nil {
			return false, nil, err
		}
	case *AugmentedUnstructured:
		review, err = augmentedUnstructuredToAdmissionRequest(*data)
		if err != nil {
			return false, nil, err
		}
	case unstructured.Unstructured:
		review, err = unstructuredToAdmissionRequest(&data)
		if err != nil {
			return false, nil, err
		}
	case *unstructured.Unstructured:
		review, err = unstructuredToAdmissionRequest(data)
		if err != nil {
			return false, nil, err
		}
	default:
		return false, nil, nil
	}

	if err := setObjectOnDelete(review); err != nil {
		return false, nil, err
	}

	return true, review, nil
}

func augmentedUnstructuredToAdmissionRequest(obj AugmentedUnstructured) (*gkReview, error) {
	review, err := unstructuredToAdmissionRequest(&obj.Object)
	if err != nil {
		return nil, err
	}

	review.namespace = obj.Namespace
	review.source = obj.Source

	return review, nil
}

func unstructuredToAdmissionRequest(obj *unstructured.Unstructured) (*gkReview, error) {
	resourceJSON, err := obj.MarshalJSON()
	if err != nil {
		return nil, fmt.Errorf("%w: unable to marshal JSON encoding of object", ErrRequestObject)
	}

	req := admissionv1.AdmissionRequest{
		Kind: metav1.GroupVersionKind{
			Group:   obj.GetObjectKind().GroupVersionKind().Group,
			Version: obj.GetObjectKind().GroupVersionKind().Version,
			Kind:    obj.GetObjectKind().GroupVersionKind().Kind,
		},
		Object: runtime.RawExtension{
			Raw: resourceJSON,
		},
		Name:      obj.GetName(),
		Namespace: obj.GetNamespace(),
	}

	return &gkReview{AdmissionRequest: req}, nil
}

func (h *K8sValidationTarget) MatchSchema() apiextensions.JSONSchemaProps {
	return matchSchema()
}

func (h *K8sValidationTarget) ValidateConstraint(u *unstructured.Unstructured) error {
	labelSelector, found, err := unstructured.NestedMap(u.Object, "spec", "match", "labelSelector")
	if err != nil {
		return err
	}

	if found && labelSelector != nil {
		labelSelectorObj, err := convertToLabelSelector(labelSelector)
		if err != nil {
			return err
		}
		errorList := validation.ValidateLabelSelector(labelSelectorObj, validation.LabelSelectorValidationOptions{}, field.NewPath("spec", "labelSelector"))
		if len(errorList) > 0 {
			return errorList.ToAggregate()
		}
	}

	namespaceSelector, found, err := unstructured.NestedMap(u.Object, "spec", "match", "namespaceSelector")
	if err != nil {
		return err
	}

	if found && namespaceSelector != nil {
		namespaceSelectorObj, err := convertToLabelSelector(namespaceSelector)
		if err != nil {
			return err
		}
		errorList := validation.ValidateLabelSelector(namespaceSelectorObj, validation.LabelSelectorValidationOptions{}, field.NewPath("spec", "labelSelector"))
		if len(errorList) > 0 {
			return errorList.ToAggregate()
		}
	}

	return nil
}

func convertToLabelSelector(object map[string]interface{}) (*metav1.LabelSelector, error) {
	j, err := json.Marshal(object)
	if err != nil {
		return nil, errors.Wrap(err, "Could not convert unknown object to JSON")
	}
	obj := &metav1.LabelSelector{}
	if err := json.Unmarshal(j, obj); err != nil {
		return nil, errors.Wrap(err, "Could not convert JSON to LabelSelector")
	}
	return obj, nil
}

func convertToMatch(object map[string]interface{}) (*match.Match, error) {
	j, err := json.Marshal(object)
	if err != nil {
		return nil, errors.Wrap(err, "could not convert unknown object to JSON")
	}
	obj := &match.Match{}
	if err := json.Unmarshal(j, obj); err != nil {
		return nil, errors.Wrap(err, "could not convert JSON to Match")
	}
	return obj, nil
}

// ToMatcher converts .spec.match in mutators to Matcher.
func (h *K8sValidationTarget) ToMatcher(u *unstructured.Unstructured) (constraints.Matcher, error) {
	obj, found, err := unstructured.NestedMap(u.Object, "spec", "match")
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrCreatingMatcher, err)
	}

	if found && obj != nil {
		m, err := convertToMatch(obj)
		if err != nil {
			return nil, fmt.Errorf("%w: %w", ErrCreatingMatcher, err)
		}
		return &Matcher{match: m, cache: &h.cache}, nil
	}

	return &Matcher{}, nil
}

func (h *K8sValidationTarget) GetCache() handler.Cache {
	return &h.cache
}

// setObjectOnDelete enforces that we use at least K8s API v1.15.0+ on DELETE operations
// and copies over the oldObject into the Object field for the given AdmissionRequest.
func setObjectOnDelete(review *gkReview) error {
	// Directly accessing the Operation field from AdmissionRequest, as it is embedded within gkReview.
	if review.Operation == admissionv1.Delete {
		// oldObject is the existing object.
		// It is null for DELETE operations in API servers prior to v1.15.0.
		// https://github.com/kubernetes/website/pull/14671
		if review.OldObject.Raw == nil {
			return ErrOldObjectIsNil
		}

		// For admission webhooks registered for DELETE operations on k8s built APIs or CRDs,
		// the apiserver now sends the existing object as admissionRequest.Request.OldObject to the webhook
		// object is the new object being admitted.
		// It is null for DELETE operations.
		// https://github.com/kubernetes/kubernetes/pull/76346
		review.Object = review.OldObject
	}
	return nil
}
