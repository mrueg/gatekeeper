/*

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package readiness_test

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/onsi/gomega"
	externaldatav1alpha1 "github.com/open-policy-agent/frameworks/constraint/pkg/apis/externaldata/v1alpha1"
	"github.com/open-policy-agent/frameworks/constraint/pkg/apis/templates/v1beta1"
	constraintclient "github.com/open-policy-agent/frameworks/constraint/pkg/client"
	"github.com/open-policy-agent/frameworks/constraint/pkg/client/drivers/local"
	frameworksexternaldata "github.com/open-policy-agent/frameworks/constraint/pkg/externaldata"
	"github.com/open-policy-agent/gatekeeper/pkg/controller"
	"github.com/open-policy-agent/gatekeeper/pkg/controller/config/process"
	"github.com/open-policy-agent/gatekeeper/pkg/externaldata"
	"github.com/open-policy-agent/gatekeeper/pkg/fakes"
	"github.com/open-policy-agent/gatekeeper/pkg/mutation"
	mutationtypes "github.com/open-policy-agent/gatekeeper/pkg/mutation/types"
	"github.com/open-policy-agent/gatekeeper/pkg/readiness"
	"github.com/open-policy-agent/gatekeeper/pkg/target"
	"github.com/open-policy-agent/gatekeeper/pkg/watch"
	"github.com/open-policy-agent/gatekeeper/test/testutils"
	"github.com/open-policy-agent/gatekeeper/third_party/sigs.k8s.io/controller-runtime/pkg/dynamiccache"
	"github.com/prometheus/client_golang/prometheus"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/rest"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/apiutil"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/metrics"
)

// setupManager sets up a controller-runtime manager with registered watch manager.
func setupManager(t *testing.T) (manager.Manager, *watch.Manager) {
	t.Helper()

	logger := zap.New(zap.UseDevMode(true), zap.WriteTo(testutils.NewTestWriter(t)))
	metrics.Registry = prometheus.NewRegistry()
	mgr, err := manager.New(cfg, manager.Options{
		HealthProbeBindAddress: "127.0.0.1:29090",
		MetricsBindAddress:     "0",
		NewCache:               dynamiccache.New,
		MapperProvider: func(c *rest.Config) (meta.RESTMapper, error) {
			return apiutil.NewDynamicRESTMapper(c)
		},
		Logger: logger,
	})
	if err != nil {
		t.Fatalf("setting up controller manager: %s", err)
	}
	c := mgr.GetCache()
	dc, ok := c.(watch.RemovableCache)
	if !ok {
		t.Fatalf("expected dynamic cache, got: %T", c)
	}
	wm, err := watch.New(dc)
	if err != nil {
		t.Fatalf("could not create watch manager: %s", err)
	}
	if err := mgr.Add(wm); err != nil {
		t.Fatalf("could not add watch manager to manager: %s", err)
	}
	return mgr, wm
}

func setupOpa(t *testing.T) *constraintclient.Client {
	// initialize OPA
	driver := local.New(local.Tracing(false))
	client, err := constraintclient.NewClient(constraintclient.Targets(&target.K8sValidationTarget{}), constraintclient.Driver(driver))
	if err != nil {
		t.Fatalf("setting up OPA client: %v", err)
	}
	return client
}

func setupController(
	mgr manager.Manager,
	wm *watch.Manager,
	opa *constraintclient.Client,
	mutationSystem *mutation.System,
	providerCache *frameworksexternaldata.ProviderCache) error {
	tracker, err := readiness.SetupTracker(mgr, mutationSystem != nil, providerCache != nil)
	if err != nil {
		return fmt.Errorf("setting up tracker: %w", err)
	}

	// ControllerSwitch will be used to disable controllers during our teardown process,
	// avoiding conflicts in finalizer cleanup.
	sw := watch.NewSwitch()

	pod := fakes.Pod(
		fakes.WithNamespace("gatekeeper-system"),
		fakes.WithName("no-pod"),
	)

	processExcluder := process.Get()

	// Setup all Controllers
	opts := controller.Dependencies{
		Opa:              opa,
		WatchManger:      wm,
		ControllerSwitch: sw,
		Tracker:          tracker,
		GetPod:           func(ctx context.Context) (*corev1.Pod, error) { return pod, nil },
		ProcessExcluder:  processExcluder,
		MutationSystem:   mutationSystem,
		ProviderCache:    providerCache,
	}
	ctx := context.Background()
	if err := controller.AddToManager(ctx, mgr, opts); err != nil {
		return fmt.Errorf("registering controllers: %w", err)
	}
	return nil
}

func Test_AssignMetadata(t *testing.T) {
	testutils.Setenv(t, "POD_NAME", "no-pod")

	// Apply fixtures *before* the controllers are setup.
	err := applyFixtures("testdata")
	if err != nil {
		t.Fatalf("applying fixtures: %v", err)
	}

	// Wire up the rest.
	mgr, wm := setupManager(t)
	opaClient := setupOpa(t)

	mutationSystem := mutation.NewSystem(mutation.SystemOpts{})

	if err := setupController(mgr, wm, opaClient, mutationSystem, nil); err != nil {
		t.Fatalf("setupControllers: %v", err)
	}

	ctx := context.Background()
	testutils.StartManager(ctx, t, mgr)

	g := gomega.NewWithT(t)
	g.Eventually(func() (bool, error) {
		ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
		defer cancel()
		return probeIsReady(ctx)
	}, 30*time.Second, 1*time.Second).Should(gomega.BeTrue())

	// Verify that the AssignMetadata is present in the cache
	for _, am := range testAssignMetadata {
		id := mutationtypes.MakeID(am)
		expectedMutator := mutationSystem.Get(id)

		if expectedMutator == nil {
			t.Errorf("got Get(%v) = nil, want non-nil", id)
		}
	}
}

func Test_ModifySet(t *testing.T) {
	g := gomega.NewWithT(t)

	testutils.Setenv(t, "POD_NAME", "no-pod")

	// Apply fixtures *before* the controllers are setup.
	err := applyFixtures("testdata")
	if err != nil {
		t.Fatalf("applying fixtures: %v", err)
	}

	// Wire up the rest.
	mgr, wm := setupManager(t)
	opaClient := setupOpa(t)

	mutationSystem := mutation.NewSystem(mutation.SystemOpts{})

	if err := setupController(mgr, wm, opaClient, mutationSystem, nil); err != nil {
		t.Fatalf("setupControllers: %v", err)
	}

	ctx := context.Background()
	testutils.StartManager(ctx, t, mgr)

	g.Eventually(func() (bool, error) {
		ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
		defer cancel()
		return probeIsReady(ctx)
	}, 20*time.Second, 1*time.Second).Should(gomega.BeTrue())

	// Verify that the ModifySet is present in the cache
	for _, am := range testModifySet {
		id := mutationtypes.MakeID(am)
		expectedMutator := mutationSystem.Get(id)
		if expectedMutator == nil {
			t.Fatal("want expectedMutator != nil but got nil")
		}
	}
}

func Test_Assign(t *testing.T) {
	g := gomega.NewWithT(t)

	testutils.Setenv(t, "POD_NAME", "no-pod")

	// Apply fixtures *before* the controllers are setup.
	err := applyFixtures("testdata")
	if err != nil {
		t.Fatalf("applying fixtures: %v", err)
	}

	// Wire up the rest.
	mgr, wm := setupManager(t)
	opaClient := setupOpa(t)

	mutationSystem := mutation.NewSystem(mutation.SystemOpts{})

	if err := setupController(mgr, wm, opaClient, mutationSystem, nil); err != nil {
		t.Fatalf("setupControllers: %v", err)
	}

	ctx := context.Background()
	testutils.StartManager(ctx, t, mgr)

	g.Eventually(func() (bool, error) {
		ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
		defer cancel()
		return probeIsReady(ctx)
	}, 20*time.Second, 1*time.Second).Should(gomega.BeTrue())

	// Verify that the Assign is present in the cache
	for _, am := range testAssign {
		id := mutationtypes.MakeID(am)
		expectedMutator := mutationSystem.Get(id)
		if expectedMutator == nil {
			t.Fatal("want expectedMutator != nil but got nil")
		}
	}
}

func Test_Provider(t *testing.T) {
	g := gomega.NewWithT(t)

	defer func() {
		externalDataEnabled := false
		externaldata.ExternalDataEnabled = &externalDataEnabled
	}()

	externalDataEnabled := true
	externaldata.ExternalDataEnabled = &externalDataEnabled

	providerCache := frameworksexternaldata.NewCache()

	err := os.Setenv("POD_NAME", "no-pod")
	if err != nil {
		t.Fatal(err)
	}
	// Apply fixtures *before* the controllers are setup.
	err = applyFixtures("testdata")
	if err != nil {
		t.Fatalf("applying fixtures: %v", err)
	}

	// Wire up the rest.
	mgr, wm := setupManager(t)
	opaClient := setupOpa(t)

	if err := setupController(mgr, wm, opaClient, mutation.NewSystem(mutation.SystemOpts{}), providerCache); err != nil {
		t.Fatalf("setupControllers: %v", err)
	}

	ctx := context.Background()
	testutils.StartManager(ctx, t, mgr)

	g.Eventually(func() (bool, error) {
		ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
		defer cancel()
		return probeIsReady(ctx)
	}, 20*time.Second, 1*time.Second).Should(gomega.BeTrue())

	// Verify that the Provider is present in the cache
	for _, tp := range testProvider {
		instance, err := providerCache.Get(tp.Name)
		if err != nil {
			t.Fatal(err)
		}

		want := externaldatav1alpha1.ProviderSpec{
			URL:     "http://demo",
			Timeout: 1,
		}
		if diff := cmp.Diff(want, instance.Spec); diff != "" {
			t.Fatal(diff)
		}
	}
}

// Test_Tracker verifies that once an initial set of fixtures are loaded into OPA,
// the readiness probe reflects that Gatekeeper is ready to enforce policy. Adding
// additional constraints afterwards will not change the readiness state.
//
// Fixtures are loaded from testdata/ and testdata/post.
// CRDs are loaded from testdata/crds (see TestMain).
// Corresponding expectations are in testdata_test.go.
func Test_Tracker(t *testing.T) {
	g := gomega.NewWithT(t)

	testutils.Setenv(t, "POD_NAME", "no-pod")

	// Apply fixtures *before* the controllers are setup.
	err := applyFixtures("testdata")
	if err != nil {
		t.Fatalf("applying fixtures: %v", err)
	}

	// Wire up the rest.
	mgr, wm := setupManager(t)
	opaClient := setupOpa(t)

	if err := setupController(mgr, wm, opaClient, mutation.NewSystem(mutation.SystemOpts{}), nil); err != nil {
		t.Fatalf("setupControllers: %v", err)
	}

	ctx := context.Background()
	testutils.StartManager(ctx, t, mgr)

	// creating the gatekeeper-system namespace is necessary because that's where
	// status resources live by default
	if err := createGatekeeperNamespace(mgr.GetConfig()); err != nil {
		t.Fatalf("want createGatekeeperNamespace(mgr.GetConfig()) error = nil, got %v", err)
	}

	g.Eventually(func() (bool, error) {
		ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
		defer cancel()
		return probeIsReady(ctx)
	}, 20*time.Second, 1*time.Second).Should(gomega.BeTrue())

	// Verify cache (tracks testdata fixtures)
	for _, ct := range testTemplates {
		_, err := opaClient.GetTemplate(ct)
		if err != nil {
			t.Fatalf("checking cache for template: %v", err)
		}
	}
	for _, c := range testConstraints {
		_, err := opaClient.GetConstraint(c)
		if err != nil {
			t.Fatalf("checking cache for constraint: %v", err)
		}
	}
	// TODO: Verify data if we add the corresponding API to opa.Client.
	// for _, d := range testData {
	// 	_, err := opaClient.GetData(ctx, c)
	// 	if err != nil {
	// t.Fatalf("checking cache for constraint: %v", err)
	// }

	// Add additional templates/constraints and verify that we remain satisfied
	err = applyFixtures("testdata/post")
	if err != nil {
		t.Fatalf("applying post fixtures: %v", err)
	}

	g.Eventually(func() (bool, error) {
		// Verify cache (tracks testdata/post fixtures)
		for _, ct := range postTemplates {
			_, err := opaClient.GetTemplate(ct)
			if err != nil {
				return false, err
			}
		}
		for _, c := range postConstraints {
			_, err := opaClient.GetConstraint(c)
			if err != nil {
				return false, err
			}
		}

		return true, nil
	}, 20*time.Second, 100*time.Millisecond).Should(gomega.BeTrue(), "verifying cache for post-fixtures")

	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
	t.Cleanup(cancel)

	ready, err := probeIsReady(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !ready {
		t.Fatal("probe should become ready after adding additional constraints")
	}
}

// Verifies that a Config resource referencing bogus (unregistered) GVKs will not halt readiness.
func Test_Tracker_UnregisteredCachedData(t *testing.T) {
	g := gomega.NewWithT(t)

	testutils.Setenv(t, "POD_NAME", "no-pod")

	// Apply fixtures *before* the controllers are setup.
	err := applyFixtures("testdata")
	if err != nil {
		t.Fatalf("applying fixtures: %v", err)
	}

	// Apply config resource with bogus GVK reference
	err = applyFixtures("testdata/bogus-config")
	if err != nil {
		t.Fatalf("applying config: %v", err)
	}

	// Wire up the rest.
	mgr, wm := setupManager(t)
	opaClient := setupOpa(t)
	if err := setupController(mgr, wm, opaClient, mutation.NewSystem(mutation.SystemOpts{}), nil); err != nil {
		t.Fatalf("setupControllers: %v", err)
	}

	ctx := context.Background()
	testutils.StartManager(ctx, t, mgr)

	g.Eventually(func() (bool, error) {
		ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
		defer cancel()
		return probeIsReady(ctx)
	}, 20*time.Second, 1*time.Second).Should(gomega.BeTrue())
}

// Test_CollectDeleted adds resources and starts the readiness tracker, then
// deletes the expected resources and ensures that the trackers watching these
// resources correctly identify the deletions and remove the corresponding expectations.
// Note that the main controllers are not running in order to target testing to the
// readiness tracker.
func Test_CollectDeleted(t *testing.T) {
	type test struct {
		description string
		gvk         schema.GroupVersionKind
		tracker     readiness.Expectations
	}

	g := gomega.NewWithT(t)

	err := applyFixtures("testdata")
	if err != nil {
		t.Fatalf("applying fixtures: %v", err)
	}

	mgr, _ := setupManager(t)

	// Setup tracker with namespaced client to avoid "noise" (control-plane-managed configmaps) from kube-system
	lister := namespacedLister{
		lister:    mgr.GetAPIReader(),
		namespace: "gatekeeper-system",
	}
	tracker := readiness.NewTracker(lister, false, false)
	err = mgr.Add(manager.RunnableFunc(func(ctx context.Context) error {
		return tracker.Run(ctx)
	}))
	if err != nil {
		t.Fatalf("setting up tracker: %v", err)
	}

	ctx := context.Background()
	testutils.StartManager(ctx, t, mgr)

	client := mgr.GetClient()

	if tracker.Satisfied() {
		t.Fatal("checking the overall tracker is unsatisfied")
	}

	// set up expected GVKs for tests
	cgvk := schema.GroupVersionKind{
		Group:   "constraints.gatekeeper.sh",
		Version: "v1beta1",
		Kind:    "K8sRequiredLabels",
	}

	cm := &corev1.ConfigMap{}
	cmgvk, err := apiutil.GVKForObject(cm, mgr.GetScheme())
	if err != nil {
		t.Fatalf("retrieving ConfigMap GVK: %v", err)
	}
	cmtracker := tracker.ForData(cmgvk)

	ct := &v1beta1.ConstraintTemplate{}
	ctgvk, err := apiutil.GVKForObject(ct, mgr.GetScheme())
	if err != nil {
		t.Fatalf("retrieving ConstraintTemplate GVK: %v", err)
	}

	// note: state can leak between these test cases because we do not reset the environment
	// between them to keep the test short. Trackers are mostly independent per GVK.
	tests := []test{
		{description: "constraints", gvk: cgvk},
		{description: "data (configmaps)", gvk: cmgvk, tracker: cmtracker},
		{description: "templates", gvk: ctgvk},
		// no need to check Config here since it is not actually Expected for readiness
		// (the objects identified in a Config's syncOnly are Expected, tested in data case above)
	}

	for _, tc := range tests {
		t.Run(tc.description, func(t *testing.T) {
			var tt readiness.Expectations
			if tc.tracker != nil {
				tt = tc.tracker
			} else {
				tt = tracker.For(tc.gvk)
			}

			g.Eventually(func() (bool, error) {
				return tt.Populated() && !tt.Satisfied(), nil
			}, 20*time.Second, 1*time.Second).
				Should(gomega.BeTrue(), "checking the tracker is tracking %s correctly")

			ul := &unstructured.UnstructuredList{}
			ul.SetGroupVersionKind(tc.gvk)
			err = lister.List(ctx, ul)
			if err != nil {
				t.Fatalf("deleting all %s", tc.description)
			}
			if len(ul.Items) == 0 {
				t.Fatal("want items to be nonempty")
			}

			for index := range ul.Items {
				err = client.Delete(ctx, &ul.Items[index])
				if err != nil {
					t.Fatalf("deleting %s %s", tc.description, ul.Items[index].GetName())
				}
			}

			g.Eventually(func() (bool, error) {
				return tt.Satisfied(), nil
			}, 20*time.Second, 1*time.Second).
				Should(gomega.BeTrue(), "checking the tracker collects deletes of %s")
		})
	}
}

// probeIsReady checks whether expectations have been satisfied (via the readiness probe).
func probeIsReady(ctx context.Context) (bool, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://127.0.0.1:29090/readyz", http.NoBody)
	if err != nil {
		return false, fmt.Errorf("constructing http request: %w", err)
	}

	resp, err := http.DefaultClient.Do(req)
	if resp != nil {
		defer resp.Body.Close()
	}
	if err != nil {
		return false, err
	}

	return resp.StatusCode >= 200 && resp.StatusCode < 400, nil
}

// namespacedLister scopes a lister to a particular namespace.
type namespacedLister struct {
	namespace string
	lister    readiness.Lister
}

func (n namespacedLister) List(ctx context.Context, list ctrlclient.ObjectList, opts ...ctrlclient.ListOption) error {
	if n.namespace != "" {
		opts = append(opts, ctrlclient.InNamespace(n.namespace))
	}
	return n.lister.List(ctx, list, opts...)
}
