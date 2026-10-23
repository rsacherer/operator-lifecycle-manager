package catalog

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/sirupsen/logrus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apiextensionsfake "k8s.io/apiextensions-apiserver/pkg/client/clientset/clientset/fake"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	fakedynamic "k8s.io/client-go/dynamic/fake"
	k8sfake "k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/tools/record"
	apiregistrationfake "k8s.io/kube-aggregator/pkg/client/clientset_generated/clientset/fake"

	operatorsv1alpha1 "github.com/operator-framework/api/pkg/operators/v1alpha1"
	"github.com/operator-framework/operator-lifecycle-manager/pkg/lib/operatorclient"
	"github.com/operator-framework/operator-lifecycle-manager/pkg/lib/operatorlister/operatorlisterfakes"
)

// pwStaticResolver implements ManifestResolver and always returns the same manifest.
type pwStaticResolver struct{ manifest string }

func (r pwStaticResolver) ManifestForStep(_ *operatorsv1alpha1.Step) (string, error) {
	return r.manifest, nil
}

const (
	pwTestCRDName   = "tests.example.com"
	pwTestCSVName   = "test-operator.v1.0.0"
	pwTestNamespace = "test-namespace"
)

// pwBuildCRD returns a minimal CRD with the given versions, conversion, and optional annotations.
func pwBuildCRD(name string, conversion *apiextensionsv1.CustomResourceConversion, versions ...apiextensionsv1.CustomResourceDefinitionVersion) *apiextensionsv1.CustomResourceDefinition {
	return &apiextensionsv1.CustomResourceDefinition{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "apiextensions.k8s.io/v1",
			Kind:       "CustomResourceDefinition",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name: name,
		},
		Spec: apiextensionsv1.CustomResourceDefinitionSpec{
			Group: "example.com",
			Names: apiextensionsv1.CustomResourceDefinitionNames{
				Plural: "tests",
				Kind:   "Test",
			},
			Scope:      apiextensionsv1.ClusterScoped,
			Versions:   versions,
			Conversion: conversion,
		},
	}
}

func pwVersion(name string, storage bool) apiextensionsv1.CustomResourceDefinitionVersion {
	return apiextensionsv1.CustomResourceDefinitionVersion{
		Name:    name,
		Served:  true,
		Storage: storage,
		Schema: &apiextensionsv1.CustomResourceValidation{
			OpenAPIV3Schema: &apiextensionsv1.JSONSchemaProps{Type: "object"},
		},
	}
}

func pwWebhookConversion(serviceName, namespace string) *apiextensionsv1.CustomResourceConversion {
	port := int32(443)
	path := "/convert"
	return &apiextensionsv1.CustomResourceConversion{
		Strategy: apiextensionsv1.WebhookConverter,
		Webhook: &apiextensionsv1.WebhookConversion{
			ClientConfig: &apiextensionsv1.WebhookClientConfig{
				Service: &apiextensionsv1.ServiceReference{
					Name:      serviceName,
					Namespace: namespace,
					Path:      &path,
					Port:      &port,
				},
				CABundle: []byte("fake-ca"),
			},
			ConversionReviewVersions: []string{"v1"},
		},
	}
}

func pwBuildBuilder(t *testing.T, currentCRD *apiextensionsv1.CustomResourceDefinition, newCRD *apiextensionsv1.CustomResourceDefinition) (*builder, *apiextensionsfake.Clientset) {
	t.Helper()

	crdFake := apiextensionsfake.NewSimpleClientset(currentCRD)
	opClient := operatorclient.NewClient(
		k8sfake.NewSimpleClientset(),
		crdFake,
		apiregistrationfake.NewSimpleClientset(),
	)

	gvrToListKind := map[schema.GroupVersionResource]string{
		{Group: "example.com", Version: "v1", Resource: "tests"}: "TestList",
		{Group: "example.com", Version: "v2", Resource: "tests"}: "TestList",
		{Group: "example.com", Version: "v3", Resource: "tests"}: "TestList",
	}
	dynamicClient := fakedynamic.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), gvrToListKind)

	manifest, err := json.Marshal(newCRD)
	require.NoError(t, err)

	csvFakeLister := &operatorlisterfakes.FakeClusterServiceVersionLister{}
	plan := &operatorsv1alpha1.InstallPlan{}
	logger := logrus.NewEntry(logrus.New())
	recorder := record.NewFakeRecorder(10)

	b := newBuilder(plan, csvFakeLister, opClient, dynamicClient,
		pwStaticResolver{manifest: string(manifest)}, logger, recorder)
	return b, crdFake
}

func pwStep(crdName string) operatorsv1alpha1.Step {
	return operatorsv1alpha1.Step{
		Status: operatorsv1alpha1.StepStatusUnknown,
		Resource: operatorsv1alpha1.StepResource{
			Kind:    crdKind,
			Name:    crdName,
			Version: "v1",
		},
	}
}

// TestPreserveLiveConversionWebhook verifies that when the bundle CRD has no
// conversion webhook (stripped by OCPBUGS-106160) but the live cluster CRD has
// an active conversion webhook, catalog-operator preserves the live webhook
// config rather than overwriting with None.
func TestPreserveLiveConversionWebhook_BundleNilConversion_LiveHasWebhook(t *testing.T) {
	// Current live CRD: storage=v2, has a running conversion webhook.
	liveWebhook := pwWebhookConversion("old-operator-service", pwTestNamespace)
	currentCRD := pwBuildCRD(pwTestCRDName, liveWebhook, pwVersion("v2", true))
	currentCRD.Status.StoredVersions = []string{"v2"}

	// Bundle CRD: promotes storage to v3, conversion stripped (nil — OCPBUGS-106160 fix).
	bundleCRD := pwBuildCRD(pwTestCRDName, nil, pwVersion("v2", false), pwVersion("v3", true))

	b, crdFake := pwBuildBuilder(t, currentCRD, bundleCRD)
	step := pwStep(pwTestCRDName)

	s, err := b.create(step)
	require.NoError(t, err)
	_, err = s.Status()
	require.NoError(t, err)

	updated, err := crdFake.ApiextensionsV1().CustomResourceDefinitions().Get(
		context.TODO(), pwTestCRDName, metav1.GetOptions{})
	require.NoError(t, err)

	require.NotNil(t, updated.Spec.Conversion,
		"conversion should not be nil — live webhook should have been preserved")
	assert.Equal(t, apiextensionsv1.WebhookConverter, updated.Spec.Conversion.Strategy,
		"strategy should be Webhook from the live CRD, not None")
	assert.Equal(t, "old-operator-service",
		updated.Spec.Conversion.Webhook.ClientConfig.Service.Name,
		"service name should match the live webhook, not be overwritten")
}

// TestPreserveLiveConversionWebhook_NoLiveWebhook_KeepsNil verifies that when
// the live CRD has no conversion webhook, behavior is unchanged (conversion stays nil).
func TestPreserveLiveConversionWebhook_NoLiveWebhook_KeepsNil(t *testing.T) {
	// Current live CRD: storage=v1, no conversion webhook.
	currentCRD := pwBuildCRD(pwTestCRDName, nil, pwVersion("v1", true))
	currentCRD.Status.StoredVersions = []string{"v1"}

	// Bundle CRD: adds v2 as storage, conversion stripped (nil).
	bundleCRD := pwBuildCRD(pwTestCRDName, nil, pwVersion("v1", false), pwVersion("v2", true))

	b, crdFake := pwBuildBuilder(t, currentCRD, bundleCRD)
	step := pwStep(pwTestCRDName)

	s, err := b.create(step)
	require.NoError(t, err)
	_, err = s.Status()
	require.NoError(t, err)

	updated, err := crdFake.ApiextensionsV1().CustomResourceDefinitions().Get(
		context.TODO(), pwTestCRDName, metav1.GetOptions{})
	require.NoError(t, err)

	// No live webhook to preserve — conversion should remain nil (unchanged behavior).
	if updated.Spec.Conversion != nil {
		assert.NotEqual(t, apiextensionsv1.WebhookConverter, updated.Spec.Conversion.Strategy,
			"conversion should not be Webhook when live CRD had none")
	}
}

// TestPreserveLiveConversionWebhook_BundleAlreadyHasWebhook_NotOverwritten verifies
// that when the bundle CRD carries its own conversion webhook (strategy=Webhook),
// the existing bundle config is used and the live webhook is NOT copied over it.
func TestPreserveLiveConversionWebhook_BundleAlreadyHasWebhook_NotOverwritten(t *testing.T) {
	liveWebhook := pwWebhookConversion("old-service", pwTestNamespace)
	currentCRD := pwBuildCRD(pwTestCRDName, liveWebhook, pwVersion("v2", true))
	currentCRD.Status.StoredVersions = []string{"v2"}

	bundleWebhook := pwWebhookConversion("new-service-from-bundle", pwTestNamespace)
	bundleCRD := pwBuildCRD(pwTestCRDName, bundleWebhook, pwVersion("v2", false), pwVersion("v3", true))

	b, crdFake := pwBuildBuilder(t, currentCRD, bundleCRD)
	step := pwStep(pwTestCRDName)

	s, err := b.create(step)
	require.NoError(t, err)
	_, err = s.Status()
	require.NoError(t, err)

	updated, err := crdFake.ApiextensionsV1().CustomResourceDefinitions().Get(
		context.TODO(), pwTestCRDName, metav1.GetOptions{})
	require.NoError(t, err)

	require.NotNil(t, updated.Spec.Conversion)
	assert.Equal(t, apiextensionsv1.WebhookConverter, updated.Spec.Conversion.Strategy)
	assert.Equal(t, "new-service-from-bundle",
		updated.Spec.Conversion.Webhook.ClientConfig.Service.Name,
		"bundle's own webhook should be preserved, not overwritten with live one")
}
