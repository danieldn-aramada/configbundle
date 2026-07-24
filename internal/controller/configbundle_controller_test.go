/*
Copyright 2026.

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

package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"sigs.k8s.io/yaml"

	armadav1 "github.com/armada/configbundle/api/v1"
)

var _ = Describe("ConfigBundle Controller", func() {
	const (
		timeout  = 10 * time.Second
		interval = 250 * time.Millisecond
	)

	ctx := context.Background()

	var (
		ns        string
		nsCounter int
	)

	BeforeEach(func() {
		nsCounter++
		ns = fmt.Sprintf("test-%d", nsCounter)
		Expect(k8sClient.Create(ctx, &corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{Name: ns},
		})).To(Succeed())
	})

	AfterEach(func() {
		// Clean up cluster-scoped CRs created in this test (they survive namespace deletion; envtest does not run GC).
		var scList armadav1.ServerConfigList
		Expect(k8sClient.List(ctx, &scList)).To(Succeed())
		for i := range scList.Items {
			Expect(k8sClient.Delete(ctx, &scList.Items[i])).To(Succeed())
		}
		var cbList armadav1.ConfigBundleList
		Expect(k8sClient.List(ctx, &cbList)).To(Succeed())
		for i := range cbList.Items {
			Expect(k8sClient.Delete(ctx, &cbList.Items[i])).To(Succeed())
		}
		Expect(k8sClient.Delete(ctx, &corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{Name: ns},
		})).To(Succeed())
	})

	Describe("child CR decomposition", func() {
		It("creates a ServerConfig named by lowercase hostname", func() {
			cb := singleServerBundle("test-bundle", "colo-r740-01", "3RK3V64", "10.10.1.45")
			Expect(k8sClient.Create(ctx, cb)).To(Succeed())

			sc := &armadav1.ServerConfig{}
			Eventually(func() error {
				return k8sClient.Get(ctx, types.NamespacedName{Name: "colo-r740-01"}, sc)
			}, timeout, interval).Should(Succeed())

			Expect(sc.Spec.ServiceTag).To(Equal("3RK3V64"))
			Expect(sc.Spec.Hostname).To(Equal(ptr.To("colo-r740-01")))
			Expect(sc.Spec.OobIP).To(Equal(ptr.To("10.10.1.45")))
		})

		It("propagates all idrac fields to the child CR", func() {
			cb := singleServerBundle("test-bundle", "colo-r740-01", "3RK3V64", "10.10.1.45")
			cb.Spec.Servers[0].IdracSettings = armadav1.IdracSettingsSpec{
				OrbID:                       "colo:srv-3rk3v64-idrac",
				FirmwareVersion:             ptr.To("7.20.10.05"),
				SSHEnabled:                  ptr.To(false),
				IPMIEnabled:                 ptr.To(false),
				LockdownModeEnabled:         ptr.To(false),
				OsToIdracPassThroughEnabled: ptr.To(false),
				UsbManagementPortEnabled:    ptr.To(true),
				DHCPEnabled:                 ptr.To(false),
				RacadmEnabled:               ptr.To(true),
			}
			Expect(k8sClient.Create(ctx, cb)).To(Succeed())

			sc := &armadav1.ServerConfig{}
			Eventually(func() error {
				return k8sClient.Get(ctx, types.NamespacedName{Name: "colo-r740-01"}, sc)
			}, timeout, interval).Should(Succeed())

			Expect(sc.Spec.IdracSettings.FirmwareVersion).To(Equal(ptr.To("7.20.10.05")))
			Expect(sc.Spec.IdracSettings.UsbManagementPortEnabled).To(Equal(ptr.To(true)))
			Expect(sc.Spec.IdracSettings.RacadmEnabled).To(Equal(ptr.To(true)))
			Expect(sc.Spec.IdracSettings.SSHEnabled).To(Equal(ptr.To(false)))
			Expect(sc.Spec.IdracSettings.IPMIEnabled).To(Equal(ptr.To(false)))
			Expect(sc.Spec.IdracSettings.DHCPEnabled).To(Equal(ptr.To(false)))
			Expect(sc.Spec.IdracSettings.LockdownModeEnabled).To(Equal(ptr.To(false)))
			Expect(sc.Spec.IdracSettings.OsToIdracPassThroughEnabled).To(Equal(ptr.To(false)))
		})

		It("creates one ServerConfig per server in a multi-server bundle", func() {
			cb := &armadav1.ConfigBundle{
				ObjectMeta: metav1.ObjectMeta{Name: "multi-galleon"},
				Spec: armadav1.ConfigBundleSpec{
					OrbID:      "colo:colo",
					Datacenter: "colo",
					Servers: []armadav1.ServerSpec{
						{OrbID: "colo:srv-3rk3v64", ServiceTag: "3RK3V64", Hostname: ptr.To("colo-r740-01"), OobIP: ptr.To("10.10.1.45")},
						{OrbID: "colo:srv-fqk3v64", ServiceTag: "FQK3V64", Hostname: ptr.To("colo-r740-02"), OobIP: ptr.To("10.10.1.46")},
					},
				},
			}
			Expect(k8sClient.Create(ctx, cb)).To(Succeed())

			for _, hostname := range []string{"colo-r740-01", "colo-r740-02"} {
				sc := &armadav1.ServerConfig{}
				Eventually(func() error {
					return k8sClient.Get(ctx, types.NamespacedName{Name: hostname}, sc)
				}, timeout, interval).Should(Succeed(), "expected ServerConfig %s to exist", hostname)
			}
		})
	})

	Describe("desired state enforcement", func() {
		It("restores a child CR field mutated out-of-band", func() {
			cb := singleServerBundle("test-bundle", "colo-r740-01", "3RK3V64", "10.10.1.45")
			cb.Spec.Servers[0].IdracSettings.SSHEnabled = ptr.To(false)
			Expect(k8sClient.Create(ctx, cb)).To(Succeed())

			// Wait for the child CR to be created.
			sc := &armadav1.ServerConfig{}
			Eventually(func() error {
				return k8sClient.Get(ctx, types.NamespacedName{Name: "colo-r740-01"}, sc)
			}, timeout, interval).Should(Succeed())

			// Simulate unauthorized drift: patch sshEnabled to true directly on the child.
			scPatched := sc.DeepCopy()
			scPatched.Spec.IdracSettings.SSHEnabled = ptr.To(true)
			Expect(k8sClient.Patch(ctx, scPatched, client.MergeFrom(sc))).To(Succeed())

			// The controller (triggered by Owns watch) should restore sshEnabled to false.
			Eventually(func(g Gomega) {
				g.Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "colo-r740-01"}, sc)).To(Succeed())
				g.Expect(sc.Spec.IdracSettings.SSHEnabled).To(Equal(ptr.To(false)))
			}, timeout, interval).Should(Succeed())
		})

		It("propagates a ConfigBundle spec update to the child CR", func() {
			cb := singleServerBundle("test-bundle", "colo-r740-01", "3RK3V64", "10.10.1.45")
			cb.Spec.Servers[0].IdracSettings.SSHEnabled = ptr.To(false)
			Expect(k8sClient.Create(ctx, cb)).To(Succeed())

			// Wait for child CR.
			sc := &armadav1.ServerConfig{}
			Eventually(func() error {
				return k8sClient.Get(ctx, types.NamespacedName{Name: "colo-r740-01"}, sc)
			}, timeout, interval).Should(Succeed())

			// Update the ConfigBundle spec — desired state changes.
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "test-bundle"}, cb)).To(Succeed())
			cb.Spec.Servers[0].IdracSettings.SSHEnabled = ptr.To(true)
			Expect(k8sClient.Update(ctx, cb)).To(Succeed())

			// Child CR must reflect the updated desired state.
			Eventually(func(g Gomega) {
				g.Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "colo-r740-01"}, sc)).To(Succeed())
				g.Expect(sc.Spec.IdracSettings.SSHEnabled).To(Equal(ptr.To(true)))
			}, timeout, interval).Should(Succeed())
		})
	})
})

// testManifest builds a minimal ConfigBundle manifest YAML for use in ConsumeServer tests.
func testManifest(datacenter string, servers ...armadav1.ServerSpec) []byte {
	spec := armadav1.ConfigBundleSpec{
		OrbID:      "colo:" + datacenter,
		Datacenter: datacenter,
		Servers:    servers,
	}
	out, err := yaml.Marshal(&spec)
	if err != nil {
		panic(fmt.Sprintf("testManifest: marshal: %v", err))
	}
	return out
}

// ---------------------------------------------------------------------------
// ConsumeServer envtest tests
// ---------------------------------------------------------------------------

var _ = Describe("ConsumeServer", func() {
	ctx := context.Background()

	var (
		ns        string
		nsCounter int
		server    *ConsumeServer
	)

	BeforeEach(func() {
		nsCounter++
		ns = fmt.Sprintf("consume-%d", nsCounter)
		Expect(k8sClient.Create(ctx, &corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{Name: ns},
		})).To(Succeed())
		server = NewConsumeServer(k8sClient,
			WithNamespace(ns),
			WithRetry(1, 0), // no retry delay in tests
		)
	})

	AfterEach(func() {
		// Clean up cluster-scoped CRs created in this test (they survive namespace deletion; envtest does not run GC).
		var scList armadav1.ServerConfigList
		Expect(k8sClient.List(ctx, &scList)).To(Succeed())
		for i := range scList.Items {
			Expect(k8sClient.Delete(ctx, &scList.Items[i])).To(Succeed())
		}
		var cbList armadav1.ConfigBundleList
		Expect(k8sClient.List(ctx, &cbList)).To(Succeed())
		for i := range cbList.Items {
			Expect(k8sClient.Delete(ctx, &cbList.Items[i])).To(Succeed())
		}
		Expect(k8sClient.Delete(ctx, &corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{Name: ns},
		})).To(Succeed())
	})

	It("creates the ConfigBundle CR and sets status on a successful dispatch", func() {
		const datacenter = "colo"
		body := testManifest(datacenter,
			armadav1.ServerSpec{OrbID: "colo:srv-3rk3v64", ServiceTag: "3RK3V64", Hostname: ptr.To("colo-r740-01"), OobIP: ptr.To("10.10.1.45")},
		)

		Expect(server.applyManifest(ctx, body, "sha256:abc123", "import-uuid-1")).To(Succeed())

		var cb armadav1.ConfigBundle
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: datacenter}, &cb)).To(Succeed())
		Expect(cb.Spec.Datacenter).To(Equal(datacenter))
		Expect(cb.Spec.Servers).To(HaveLen(1))
		Expect(cb.Spec.Servers[0].ServiceTag).To(Equal("3RK3V64"))
		Expect(cb.Status.Phase).To(Equal(armadav1.ConfigBundlePhaseApplied))
		Expect(cb.Status.LastAppliedDigest).To(Equal("sha256:abc123"))
		Expect(cb.Status.LastOrbImportID).To(Equal("import-uuid-1"))
		Expect(cb.Status.LastAppliedAt).NotTo(BeNil())
		Expect(conditionStatus(cb.Status.Conditions, armadav1.ConditionReconciled)).
			To(Equal(metav1.ConditionTrue))
	})

	It("is idempotent — applying the same manifest twice does not error", func() {
		const datacenter = "colo"
		body := testManifest(datacenter,
			armadav1.ServerSpec{OrbID: "colo:srv-3rk3v64", ServiceTag: "3RK3V64", Hostname: ptr.To("colo-r740-01"), OobIP: ptr.To("10.10.1.45")},
		)
		Expect(server.applyManifest(ctx, body, "sha256:abc", "import-1")).To(Succeed())
		Expect(server.applyManifest(ctx, body, "sha256:abc", "import-1")).To(Succeed())

		var cb armadav1.ConfigBundle
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: datacenter}, &cb)).To(Succeed())
		Expect(cb.Spec.Servers).To(HaveLen(1))
	})

	It("updates an existing CR when a new dispatch arrives", func() {
		const datacenter = "colo"
		Expect(server.applyManifest(ctx, testManifest(datacenter,
			armadav1.ServerSpec{OrbID: "colo:srv-3rk3v64", ServiceTag: "3RK3V64", Hostname: ptr.To("r1"), OobIP: ptr.To("10.0.0.1")},
		), "sha256:v1", "import-1")).To(Succeed())

		Expect(server.applyManifest(ctx, testManifest(datacenter,
			armadav1.ServerSpec{OrbID: "colo:srv-3rk3v64", ServiceTag: "3RK3V64", Hostname: ptr.To("r1"), OobIP: ptr.To("10.0.0.1")},
			armadav1.ServerSpec{OrbID: "colo:srv-fqk3v64", ServiceTag: "FQK3V64", Hostname: ptr.To("r2"), OobIP: ptr.To("10.0.0.2")},
		), "sha256:v2", "import-2")).To(Succeed())

		var cb armadav1.ConfigBundle
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: datacenter}, &cb)).To(Succeed())
		Expect(cb.Spec.Servers).To(HaveLen(2))
		Expect(cb.Status.LastAppliedDigest).To(Equal("sha256:v2"))
		Expect(cb.Status.LastOrbImportID).To(Equal("import-2"))
	})

	It("omits only admin-owned leaves; controller still updates the rest of the same server", func() {
		const datacenter = "colo"

		// Step 1: controller seeds the CR with server A's full state.
		seed := testManifest(datacenter,
			armadav1.ServerSpec{OrbID: "colo:srv-aaa0001", ServiceTag: "AAA0001", Hostname: ptr.To("colo-r740-01"), OobIP: ptr.To("10.10.1.45"),
				IdracSettings: armadav1.IdracSettingsSpec{OrbID: "colo:srv-aaa0001-idrac", SSHEnabled: ptr.To(false), FirmwareVersion: ptr.To("7.0.0"), RacadmEnabled: ptr.To(false)}},
		)
		Expect(server.applyManifest(ctx, seed, "sha256:seed", "import-0")).To(Succeed())

		// Step 2: local:admin overrides ONE leaf — idrac.sshEnabled — on server A.
		// Build the apply as unstructured so we claim ONLY the leaf we set; struct
		// marshaling would serialize zero-valued primitives and claim too much.
		adminApply := &unstructured.Unstructured{Object: map[string]any{
			"apiVersion": armadav1.GroupVersion.String(),
			"kind":       "ConfigBundle",
			"metadata":   map[string]any{"name": datacenter},
			"spec": map[string]any{
				"servers": []any{
					map[string]any{
						"orbId":         "colo:srv-aaa0001",
						"idracSettings": map[string]any{"sshEnabled": true},
					},
				},
			},
		}}
		Expect(k8sClient.Patch(ctx, adminApply, client.Apply,
			client.FieldOwner("local:admin"),
			client.ForceOwnership,
		)).To(Succeed())

		// Step 3: controller dispatch updates server A (new oobIP, new firmware, new racadm)
		// AND adds an uncontested server B. Admin's sshEnabled override on A must survive,
		// but the other fields on A must take the controller's new values.
		body := testManifest(datacenter,
			armadav1.ServerSpec{OrbID: "colo:srv-aaa0001", ServiceTag: "AAA0001", Hostname: ptr.To("colo-r740-01"), OobIP: ptr.To("10.10.1.99"),
				IdracSettings: armadav1.IdracSettingsSpec{OrbID: "colo:srv-aaa0001-idrac", SSHEnabled: ptr.To(false), FirmwareVersion: ptr.To("7.20.10.05"), RacadmEnabled: ptr.To(true)}},
			armadav1.ServerSpec{OrbID: "colo:srv-bbb0002", ServiceTag: "BBB0002", Hostname: ptr.To("colo-r740-02"), OobIP: ptr.To("10.10.1.46")},
		)
		Expect(server.applyManifest(ctx, body, "sha256:newdigest", "import-1")).To(Succeed())

		var cb armadav1.ConfigBundle
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: datacenter}, &cb)).To(Succeed())

		serverB := findServerByTag(cb.Spec.Servers, "BBB0002")
		Expect(serverB).NotTo(BeNil(), "server B must be present")

		serverA := findServerByTag(cb.Spec.Servers, "AAA0001")
		Expect(serverA).NotTo(BeNil(), "server A must still be present")

		// Admin-owned leaf preserved:
		Expect(serverA.IdracSettings.SSHEnabled).To(Equal(ptr.To(true)), "admin's sshEnabled override must be preserved")

		// Controller-updatable leaves propagated:
		Expect(serverA.OobIP).To(Equal(ptr.To("10.10.1.99")), "controller's oobIP update must take effect")
		Expect(serverA.IdracSettings.FirmwareVersion).To(Equal(ptr.To("7.20.10.05")), "controller's firmwareVersion update must take effect")
		Expect(serverA.IdracSettings.RacadmEnabled).To(Equal(ptr.To(true)), "controller's racadmEnabled update must take effect")
	})

	It("returns error when manifest has empty datacenter", func() {
		err := server.applyManifest(ctx, []byte("datacenter: \"\"\n"), "sha256:x", "import-1")
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("empty datacenter"))
	})

	It("applies a manifest with no servers", func() {
		const datacenter = "colo"
		Expect(server.applyManifest(ctx, testManifest(datacenter), "sha256:empty", "import-1")).To(Succeed())
		var cb armadav1.ConfigBundle
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: datacenter}, &cb)).To(Succeed())
		Expect(cb.Spec.Servers).To(BeEmpty())
	})

	It("retries status update on conflict against the reconciler's ObservedGeneration write", func() {
		// Regression: ConsumeServer's Status().Update races the
		// ConfigBundleReconciler's ObservedGeneration write. Without
		// RetryOnConflict, the losing writer returned "the object has been
		// modified" and the work was dropped. Two concurrent applies mirror
		// the realistic production race (dispatch + reconciler).
		const datacenter = "colo"
		body := testManifest(datacenter,
			armadav1.ServerSpec{OrbID: "colo:srv-3rk3v64", ServiceTag: "3RK3V64", Hostname: ptr.To("retry-r740-01"), OobIP: ptr.To("10.0.0.1")},
		)

		// Seed the CR so concurrent applies operate on an existing object.
		// This also drives the reconciler so its ObservedGeneration write
		// will race the subsequent applies.
		Expect(server.applyManifest(ctx, body, "sha256:seed", "import-seed")).To(Succeed())

		const goroutines = 2
		var wg sync.WaitGroup
		errCh := make(chan error, goroutines)
		for i := range goroutines {
			wg.Add(1)
			go func(idx int) {
				defer wg.Done()
				errCh <- server.applyManifest(ctx,
					body,
					fmt.Sprintf("sha256:digest-%d", idx),
					fmt.Sprintf("import-%d", idx),
				)
			}(i)
		}
		wg.Wait()
		close(errCh)

		for err := range errCh {
			Expect(err).NotTo(HaveOccurred(),
				"applyManifest must not surface IsConflict after retry")
		}

		var final armadav1.ConfigBundle
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: datacenter}, &final)).To(Succeed())
		Expect(final.Status.LastAppliedDigest).To(HavePrefix("sha256:digest-"),
			"status must reflect one of the concurrent writers")
	})
})

// conditionStatus returns the Status of the condition with the given type, or "" if absent.
func conditionStatus(conditions []metav1.Condition, condType string) metav1.ConditionStatus {
	for _, c := range conditions {
		if c.Type == condType {
			return c.Status
		}
	}
	return ""
}

var _ = Describe("SSA list merge-key isolation on servers[]", func() {
	const (
		timeout  = 10 * time.Second
		interval = 250 * time.Millisecond
	)

	ctx := context.Background()

	var (
		ns        string
		nsCounter int
	)

	BeforeEach(func() {
		nsCounter++
		ns = fmt.Sprintf("ssa-isolation-%d", nsCounter)
		Expect(k8sClient.Create(ctx, &corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{Name: ns},
		})).To(Succeed())
	})

	AfterEach(func() {
		// Clean up cluster-scoped CRs created in this test (they survive namespace deletion; envtest does not run GC).
		var scList armadav1.ServerConfigList
		Expect(k8sClient.List(ctx, &scList)).To(Succeed())
		for i := range scList.Items {
			Expect(k8sClient.Delete(ctx, &scList.Items[i])).To(Succeed())
		}
		var cbList armadav1.ConfigBundleList
		Expect(k8sClient.List(ctx, &cbList)).To(Succeed())
		for i := range cbList.Items {
			Expect(k8sClient.Delete(ctx, &cbList.Items[i])).To(Succeed())
		}
		Expect(k8sClient.Delete(ctx, &corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{Name: ns},
		})).To(Succeed())
	})

	It("local admin override on one server entry does not block Puller updates to other entries", func() {
		// Step 1: local:admin is the first applier, claiming server A.
		// With +listType=map, ownership is scoped to the individual entry by orbId.
		// Without the annotation the entire servers[] array would be owned atomically by admin,
		// and step 2 below would return 409 even though it only touches server B.
		adminApply := &armadav1.ConfigBundle{
			TypeMeta: metav1.TypeMeta{
				APIVersion: armadav1.GroupVersion.String(),
				Kind:       "ConfigBundle",
			},
			ObjectMeta: metav1.ObjectMeta{Name: "ssa-isolation-test"},
			Spec: armadav1.ConfigBundleSpec{
				OrbID:      "colo:colo",
				Datacenter: "colo",
				Servers: []armadav1.ServerSpec{
					{
						OrbID:         "colo:srv-aaa0001",
						ServiceTag:    "AAA0001",
						Hostname:      ptr.To("colo-r740-01"),
						OobIP:         ptr.To("10.10.1.45"),
						IdracSettings: armadav1.IdracSettingsSpec{OrbID: "colo:srv-aaa0001-idrac", SSHEnabled: ptr.To(true)},
					},
				},
			},
		}
		Expect(k8sClient.Patch(ctx, adminApply, client.Apply,
			client.FieldOwner("local:admin"),
		)).To(Succeed())

		// Step 2: Puller applies server B only.
		// It has inspected managedFields, found admin owns server A, and omitted that entry.
		// With +listType=map: entries are tracked independently — server B is uncontested.
		// Without the annotation: admin owns servers[] atomically → this apply returns 409.
		pullerApply := &armadav1.ConfigBundle{
			TypeMeta: metav1.TypeMeta{
				APIVersion: armadav1.GroupVersion.String(),
				Kind:       "ConfigBundle",
			},
			ObjectMeta: metav1.ObjectMeta{Name: "ssa-isolation-test"},
			Spec: armadav1.ConfigBundleSpec{
				OrbID:      "colo:colo",
				Datacenter: "colo",
				Servers: []armadav1.ServerSpec{
					{
						OrbID:         "colo:srv-bbb0002",
						ServiceTag:    "BBB0002",
						Hostname:      ptr.To("colo-r740-02"),
						OobIP:         ptr.To("10.10.1.46"),
						IdracSettings: armadav1.IdracSettingsSpec{OrbID: "colo:srv-bbb0002-idrac", SSHEnabled: ptr.To(false)},
					},
				},
			},
		}
		Expect(k8sClient.Patch(ctx, pullerApply, client.Apply,
			client.FieldOwner("configbundle-controller"),
		)).To(Succeed(), "+listType=map must scope ownership to individual entries — Puller apply of server B must not 409")

		result := &armadav1.ConfigBundle{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "ssa-isolation-test"}, result)).To(Succeed())

		serverA := findServerByTag(result.Spec.Servers, "AAA0001")
		Expect(serverA).NotTo(BeNil(), "server A (admin-owned) must still be present")
		Expect(serverA.IdracSettings.SSHEnabled).To(Equal(ptr.To(true)), "admin override on server A must be preserved")

		serverB := findServerByTag(result.Spec.Servers, "BBB0002")
		Expect(serverB).NotTo(BeNil(), "server B (Puller-owned) must be present")
		Expect(serverB.IdracSettings.SSHEnabled).To(Equal(ptr.To(false)), "Puller's desired state for server B must land")
	})
})

// ---------------------------------------------------------------------------
// Divergence Reporter envtest tests
// ---------------------------------------------------------------------------

var _ = Describe("DivergenceReporter", func() {
	const (
		timeout  = 10 * time.Second
		interval = 250 * time.Millisecond
	)

	ctx := context.Background()

	var (
		ns        string
		nsCounter int
	)

	BeforeEach(func() {
		nsCounter++
		ns = fmt.Sprintf("reporter-%d", nsCounter)
		Expect(k8sClient.Create(ctx, &corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{Name: ns},
		})).To(Succeed())
	})

	AfterEach(func() {
		// Clean up cluster-scoped CRs created in this test (they survive namespace deletion; envtest does not run GC).
		var scList armadav1.ServerConfigList
		Expect(k8sClient.List(ctx, &scList)).To(Succeed())
		for i := range scList.Items {
			Expect(k8sClient.Delete(ctx, &scList.Items[i])).To(Succeed())
		}
		var cbList armadav1.ConfigBundleList
		Expect(k8sClient.List(ctx, &cbList)).To(Succeed())
		for i := range cbList.Items {
			Expect(k8sClient.Delete(ctx, &cbList.Items[i])).To(Succeed())
		}
		Expect(k8sClient.Delete(ctx, &corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{Name: ns},
		})).To(Succeed())
	})

	It("detects a local:admin override and produces correct override entries", func() {
		const datacenter = "colo"

		// Step 1: configbundle-controller applies the initial spec (cloud intent).
		controllerApply := &armadav1.ConfigBundle{
			TypeMeta:   metav1.TypeMeta{APIVersion: armadav1.GroupVersion.String(), Kind: "ConfigBundle"},
			ObjectMeta: metav1.ObjectMeta{Name: datacenter},
			Spec: armadav1.ConfigBundleSpec{
				OrbID:      "colo:colo",
				Datacenter: datacenter,
				Servers: []armadav1.ServerSpec{
					{
						OrbID:         "colo:srv-3rk3v64",
						ServiceTag:    "3RK3V64",
						Hostname:      ptr.To("colo-r740-01"),
						OobIP:         ptr.To("10.10.1.45"),
						IdracSettings: armadav1.IdracSettingsSpec{OrbID: "colo:srv-3rk3v64-idrac", SSHEnabled: ptr.To(false), RacadmEnabled: ptr.To(true)},
					},
				},
			},
		}
		Expect(k8sClient.Patch(ctx, controllerApply, client.Apply,
			client.FieldOwner("configbundle-controller"),
		)).To(Succeed())

		// Step 2: local:admin overrides sshEnabled to true.
		// Build as unstructured so we claim only the leaf we set.
		adminApply := &unstructured.Unstructured{Object: map[string]any{
			"apiVersion": armadav1.GroupVersion.String(),
			"kind":       "ConfigBundle",
			"metadata":   map[string]any{"name": datacenter},
			"spec": map[string]any{
				"servers": []any{
					map[string]any{
						"orbId":         "colo:srv-3rk3v64",
						"idracSettings": map[string]any{"sshEnabled": true},
					},
				},
			},
		}}
		Expect(k8sClient.Patch(ctx, adminApply, client.Apply,
			client.FieldOwner("local:admin"),
			client.ForceOwnership,
		)).To(Succeed())

		// Step 3: Read the CR back to get managedFields.
		var cb armadav1.ConfigBundle
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: datacenter}, &cb)).To(Succeed())

		// Step 4: Build a reporter with a mapping and set the last manifest.
		reporter := NewDivergenceReporter(k8sClient,
			WithDivergenceNamespace(ns),
			WithDivergenceEnabled(true),
		)
		reporter.SetLastManifest(datacenter, controllerApply.Spec)

		// Step 5: Extract overrides and verify orbital-native fields.
		overrides := reporter.extractOverrides(&cb, controllerApply.Spec)
		Expect(overrides).NotTo(BeEmpty(), "should detect at least one override")

		var found *OverrideEntry
		for i := range overrides {
			if overrides[i].OrbID == "colo:srv-3rk3v64-idrac" && overrides[i].Field == "sshEnabled" {
				found = &overrides[i]
				break
			}
		}
		Expect(found).NotTo(BeNil(), "should find sshEnabled override")
		Expect(found.Type).To(Equal("IdracSettings"))
		Expect(found.OverrideValue).To(Equal(ptr.To(true)), "override value should be true")
		Expect(found.IntendedValue).To(Equal(ptr.To(false)), "intended value should be false")
		Expect(found.Who).To(Equal("local:admin"))
		Expect(found.When).NotTo(BeEmpty())
	})

	It("reports empty overrides when no local:admin fields exist", func() {
		const datacenter = "colo"

		controllerApply := &armadav1.ConfigBundle{
			TypeMeta:   metav1.TypeMeta{APIVersion: armadav1.GroupVersion.String(), Kind: "ConfigBundle"},
			ObjectMeta: metav1.ObjectMeta{Name: datacenter},
			Spec: armadav1.ConfigBundleSpec{
				OrbID:      "colo:colo",
				Datacenter: datacenter,
				Servers: []armadav1.ServerSpec{
					{OrbID: "colo:srv-3rk3v64", ServiceTag: "3RK3V64", Hostname: ptr.To("colo-r740-01"), OobIP: ptr.To("10.10.1.45")},
				},
			},
		}
		Expect(k8sClient.Patch(ctx, controllerApply, client.Apply,
			client.FieldOwner("configbundle-controller"),
		)).To(Succeed())

		var cb armadav1.ConfigBundle
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: datacenter}, &cb)).To(Succeed())

		reporter := NewDivergenceReporter(k8sClient,
			WithDivergenceNamespace(ns),
			WithDivergenceEnabled(true),
		)
		reporter.SetLastManifest(datacenter, controllerApply.Spec)

		overrides := reporter.extractOverrides(&cb, controllerApply.Spec)
		Expect(overrides).To(BeEmpty(), "no local:admin fields → no overrides")
	})

	It("POSTs the divergence payload to the intake URL", func() {
		const datacenter = "colo"

		// Set up an HTTP server to capture the POST.
		var capturedPayload DivergencePayload
		intake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			defer r.Body.Close()
			Expect(json.NewDecoder(r.Body).Decode(&capturedPayload)).To(Succeed())
			w.WriteHeader(http.StatusOK)
		}))
		defer intake.Close()

		// Step 1: controller applies initial spec.
		controllerApply := &armadav1.ConfigBundle{
			TypeMeta:   metav1.TypeMeta{APIVersion: armadav1.GroupVersion.String(), Kind: "ConfigBundle"},
			ObjectMeta: metav1.ObjectMeta{Name: datacenter},
			Spec: armadav1.ConfigBundleSpec{
				OrbID:      "colo:colo",
				Datacenter: datacenter,
				Servers: []armadav1.ServerSpec{
					{
						OrbID:         "colo:srv-3rk3v64",
						ServiceTag:    "3RK3V64",
						Hostname:      ptr.To("colo-r740-01"),
						OobIP:         ptr.To("10.10.1.45"),
						IdracSettings: armadav1.IdracSettingsSpec{OrbID: "colo:srv-3rk3v64-idrac", SSHEnabled: ptr.To(false)},
					},
				},
			},
		}
		Expect(k8sClient.Patch(ctx, controllerApply, client.Apply,
			client.FieldOwner("configbundle-controller"),
		)).To(Succeed())

		// Step 2: local:admin overrides sshEnabled.
		// Build as unstructured so we claim only the leaf we set.
		// ConfigBundle is cluster-scoped — no namespace in metadata.
		adminApply := &unstructured.Unstructured{Object: map[string]any{
			"apiVersion": armadav1.GroupVersion.String(),
			"kind":       "ConfigBundle",
			"metadata":   map[string]any{"name": datacenter},
			"spec": map[string]any{
				"servers": []any{
					map[string]any{
						"orbId":         "colo:srv-3rk3v64",
						"idracSettings": map[string]any{"sshEnabled": true},
					},
				},
			},
		}}
		Expect(k8sClient.Patch(ctx, adminApply, client.Apply,
			client.FieldOwner("local:admin"),
			client.ForceOwnership,
		)).To(Succeed())

		// Update status with a digest (informational; divergence reporter no
		// longer reads a mapping CM under ADR-011, but the status field is
		// still part of the contract). RetryOnConflict — the configbundle
		// reconciler also writes Status.ObservedGeneration in the background.
		Expect(retry.RetryOnConflict(retry.DefaultRetry, func() error {
			var cb armadav1.ConfigBundle
			if err := k8sClient.Get(ctx, types.NamespacedName{Name: datacenter}, &cb); err != nil {
				return err
			}
			cb.Status.LastAppliedDigest = "sha256:test-digest"
			return k8sClient.Status().Update(ctx, &cb)
		})).To(Succeed())

		// Step 3: Run reporter.Reconcile() directly (lastEventAt is zero → startup case, no debounce).
		reporter := NewDivergenceReporter(k8sClient,
			WithDivergenceNamespace(ns),
			WithDivergenceEnabled(true),
			WithDivergenceIntakeURL(intake.URL),
		)
		reporter.SetLastManifest(datacenter, controllerApply.Spec)

		_, err := reporter.Reconcile(ctx, reconcile.Request{
			NamespacedName: types.NamespacedName{Name: datacenter},
		})
		Expect(err).NotTo(HaveOccurred())

		// Step 5: Verify the captured payload — orbital-native, no bundleDigest.
		Expect(capturedPayload.Overrides).NotTo(BeEmpty())

		var sshOverride *OverrideEntry
		for i := range capturedPayload.Overrides {
			if capturedPayload.Overrides[i].OrbID == "colo:srv-3rk3v64-idrac" && capturedPayload.Overrides[i].Field == "sshEnabled" {
				sshOverride = &capturedPayload.Overrides[i]
				break
			}
		}
		Expect(sshOverride).NotTo(BeNil())
		Expect(sshOverride.Type).To(Equal("IdracSettings"))
		Expect(sshOverride.OverrideValue).To(Equal(true))
		Expect(sshOverride.IntendedValue).To(Equal(false))
	})
})

// ---------------------------------------------------------------------------
// Takeover envtest tests
// ---------------------------------------------------------------------------

var _ = Describe("Takeover", func() {
	const (
		timeout  = 10 * time.Second
		interval = 250 * time.Millisecond
	)

	ctx := context.Background()

	var (
		ns        string
		nsCounter int
		server    *ConsumeServer
	)

	BeforeEach(func() {
		nsCounter++
		ns = fmt.Sprintf("takeover-%d", nsCounter)
		Expect(k8sClient.Create(ctx, &corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{Name: ns},
		})).To(Succeed())
		server = NewConsumeServer(k8sClient,
			WithNamespace(ns),
			WithRetry(1, 0),
		)
	})

	AfterEach(func() {
		// Clean up cluster-scoped CRs created in this test (they survive namespace deletion; envtest does not run GC).
		var scList armadav1.ServerConfigList
		Expect(k8sClient.List(ctx, &scList)).To(Succeed())
		for i := range scList.Items {
			Expect(k8sClient.Delete(ctx, &scList.Items[i])).To(Succeed())
		}
		var cbList armadav1.ConfigBundleList
		Expect(k8sClient.List(ctx, &cbList)).To(Succeed())
		for i := range cbList.Items {
			Expect(k8sClient.Delete(ctx, &cbList.Items[i])).To(Succeed())
		}
		Expect(k8sClient.Delete(ctx, &corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{Name: ns},
		})).To(Succeed())
	})

	It("reclaims a local:admin-owned field and leaves other admin fields intact", func() {
		const datacenter = "colo"

		// Step 1: configbundle-controller applies the initial spec.
		controllerApply := &armadav1.ConfigBundle{
			TypeMeta:   metav1.TypeMeta{APIVersion: armadav1.GroupVersion.String(), Kind: "ConfigBundle"},
			ObjectMeta: metav1.ObjectMeta{Name: datacenter},
			Spec: armadav1.ConfigBundleSpec{
				OrbID:      "colo:colo",
				Datacenter: datacenter,
				Servers: []armadav1.ServerSpec{
					{
						OrbID:      "colo:srv-3rk3v64",
						ServiceTag: "3RK3V64",
						Hostname:   ptr.To("colo-r740-01"),
						OobIP:      ptr.To("10.10.1.45"),
						IdracSettings: armadav1.IdracSettingsSpec{
							SSHEnabled:    ptr.To(false),
							RacadmEnabled: ptr.To(true),
						},
					},
				},
			},
		}
		Expect(k8sClient.Patch(ctx, controllerApply, client.Apply,
			client.FieldOwner("configbundle-controller"),
		)).To(Succeed())

		// Step 2: local:admin overrides ONLY sshEnabled AND racadmEnabled on the idrac.
		// Build as unstructured so we claim ONLY the idrac leaves we set; struct marshaling
		// would serialize zero-valued non-omitempty fields (e.g. serviceTag) and claim too much.
		adminApply := &unstructured.Unstructured{Object: map[string]any{
			"apiVersion": armadav1.GroupVersion.String(),
			"kind":       "ConfigBundle",
			"metadata":   map[string]any{"name": datacenter},
			"spec": map[string]any{
				"servers": []any{
					map[string]any{
						"orbId": "colo:srv-3rk3v64",
						"idracSettings": map[string]any{
							"sshEnabled":    true,
							"racadmEnabled": false,
						},
					},
				},
			},
		}}
		Expect(k8sClient.Patch(ctx, adminApply, client.Apply,
			client.FieldOwner("local:admin"),
			client.ForceOwnership,
		)).To(Succeed())

		// Verify admin owns at least one field on the server before takeover.
		var cbBefore armadav1.ConfigBundle
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: datacenter}, &cbBefore)).To(Succeed())
		Expect(hasAdminEntryFor(cbBefore.ManagedFields, "colo:srv-3rk3v64")).To(BeTrue(),
			"admin should own at least one field on the server entry")

		// Step 3: Run takeover — reclaim ONLY sshEnabled, leave racadmEnabled with admin.
		spec := armadav1.ConfigBundleSpec{
			OrbID:      "colo:colo",
			Datacenter: datacenter,
			Servers: []armadav1.ServerSpec{
				{
					OrbID:      "colo:srv-3rk3v64",
					ServiceTag: "3RK3V64",
					Hostname:   ptr.To("colo-r740-01"),
					OobIP:      ptr.To("10.10.1.45"),
					IdracSettings: armadav1.IdracSettingsSpec{
						SSHEnabled:    ptr.To(false),
						RacadmEnabled: ptr.To(true),
					},
				},
			},
			Takeover: []armadav1.TakeoverEntry{
				{OrbID: "colo:srv-001-idrac", ServerOrbID: "colo:srv-3rk3v64", Field: "sshEnabled"},
			},
		}
		// processTakeover now consumes the admin-omitted patchSpec produced by
		// the normal-apply pass; compute it here for the test.
		var cbForPatch armadav1.ConfigBundle
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: datacenter}, &cbForPatch)).To(Succeed())
		patchSpec, err := omitAdminOwnedFields(spec, cbForPatch.Spec, cbForPatch.ManagedFields)
		Expect(err).NotTo(HaveOccurred())
		Expect(server.processTakeover(ctx, spec, patchSpec)).To(Succeed())

		// Step 4: Read the CR back and verify field values.
		var cbAfter armadav1.ConfigBundle
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: datacenter}, &cbAfter)).To(Succeed())

		srv := findServerByTag(cbAfter.Spec.Servers, "3RK3V64")
		Expect(srv).NotTo(BeNil())

		// sshEnabled should be reclaimed to controller's value (false).
		Expect(srv.IdracSettings.SSHEnabled).To(Equal(ptr.To(false)), "sshEnabled should be reclaimed to controller intent (false)")

		// racadmEnabled should still be the admin's override value (false).
		Expect(srv.IdracSettings.RacadmEnabled).To(Equal(ptr.To(false)), "racadmEnabled should still be admin's override value")

		// Verify managedFields: sshEnabled should now be owned by configbundle-controller.
		// local:admin should still own racadmEnabled but NOT sshEnabled.
		adminPaths := extractAdminPaths(cbAfter.ManagedFields)
		var adminOwnsSSH, adminOwnsRacadm bool
		for _, ap := range adminPaths {
			if ap.path == "spec.servers[orbId=colo:srv-3rk3v64].idracSettings.sshEnabled" {
				adminOwnsSSH = true
			}
			if ap.path == "spec.servers[orbId=colo:srv-3rk3v64].idracSettings.racadmEnabled" {
				adminOwnsRacadm = true
			}
		}
		Expect(adminOwnsSSH).To(BeFalse(), "local:admin should no longer own sshEnabled after takeover")
		Expect(adminOwnsRacadm).To(BeTrue(), "local:admin should still own racadmEnabled (not targeted by takeover)")
	})

	It("succeeds with empty takeover list", func() {
		spec := armadav1.ConfigBundleSpec{OrbID: "colo:colo", Datacenter: "colo"}
		patchSpec := spec.DeepCopy()
		Expect(server.processTakeover(ctx, spec, patchSpec)).To(Succeed())
	})

	It("returns error when targeting a nonexistent server", func() {
		const datacenter = "colo"

		controllerApply := &armadav1.ConfigBundle{
			TypeMeta:   metav1.TypeMeta{APIVersion: armadav1.GroupVersion.String(), Kind: "ConfigBundle"},
			ObjectMeta: metav1.ObjectMeta{Name: datacenter},
			Spec: armadav1.ConfigBundleSpec{
				OrbID:      "colo:colo",
				Datacenter: datacenter,
				Servers: []armadav1.ServerSpec{
					{OrbID: "colo:srv-3rk3v64", ServiceTag: "3RK3V64", Hostname: ptr.To("colo-r740-01"), OobIP: ptr.To("10.10.1.45")},
				},
			},
		}
		Expect(k8sClient.Patch(ctx, controllerApply, client.Apply,
			client.FieldOwner("configbundle-controller"),
		)).To(Succeed())

		spec := armadav1.ConfigBundleSpec{
			OrbID:      "colo:colo",
			Datacenter: datacenter,
			Servers:    controllerApply.Spec.Servers,
			Takeover: []armadav1.TakeoverEntry{
				{OrbID: "x", ServerOrbID: "colo:srv-nonexistent", Field: "sshEnabled"},
			},
		}
		patchSpec := spec.DeepCopy()
		err := server.processTakeover(ctx, spec, patchSpec)
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("takeover entries failed"))
	})
})

// ---------------------------------------------------------------------------
// Prune tests
// ---------------------------------------------------------------------------

var _ = Describe("ConfigBundle Controller — orphan pruning", func() {
	const (
		timeout  = 10 * time.Second
		interval = 250 * time.Millisecond
	)

	ctx := context.Background()

	AfterEach(func() {
		for _, list := range []client.ObjectList{
			&armadav1.BackupConfigList{},
			&armadav1.ServerConfigList{},
			&armadav1.ConfigBundleList{},
		} {
			Expect(k8sClient.List(ctx, list)).To(Succeed())
			switch l := list.(type) {
			case *armadav1.BackupConfigList:
				for i := range l.Items {
					Expect(k8sClient.Delete(ctx, &l.Items[i])).To(Or(Succeed(), MatchError(ContainSubstring("not found"))))
				}
			case *armadav1.ServerConfigList:
				for i := range l.Items {
					Expect(k8sClient.Delete(ctx, &l.Items[i])).To(Or(Succeed(), MatchError(ContainSubstring("not found"))))
				}
			case *armadav1.ConfigBundleList:
				for i := range l.Items {
					Expect(k8sClient.Delete(ctx, &l.Items[i])).To(Or(Succeed(), MatchError(ContainSubstring("not found"))))
				}
			}
		}
	})

	It("deletes a BackupConfig when its cluster backup is removed from the spec", func() {
		cb := &armadav1.ConfigBundle{
			ObjectMeta: metav1.ObjectMeta{Name: "prune-test"},
			Spec: armadav1.ConfigBundleSpec{
				OrbID:      "colo:prune-test",
				Datacenter: "colo",
				KubernetesClusters: []armadav1.KubernetesClusterSpec{{
					OrbID: "colo:cluster-a",
					Backup: &armadav1.ClusterBackupSpec{
						OrbID: "colo:cluster-a-backup",
						Etcd: &armadav1.EtcdBackupSpec{
							OrbID:   "colo:cluster-a-etcd",
							Enabled: ptr.To(true),
						},
					},
				}},
			},
		}
		Expect(k8sClient.Create(ctx, cb)).To(Succeed())

		bc := &armadav1.BackupConfig{}
		Eventually(func() error {
			return k8sClient.Get(ctx, types.NamespacedName{Name: "colo-cluster-a-backup"}, bc)
		}, timeout, interval).Should(Succeed(), "BackupConfig must be created initially")

		// Remove the backup block — simulates the cluster's ClusterBackup node being
		// deleted from orbital and dropped from the next bundle.
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "prune-test"}, cb)).To(Succeed())
		cb.Spec.KubernetesClusters[0].Backup = nil
		Expect(k8sClient.Update(ctx, cb)).To(Succeed())

		Eventually(func() error {
			return k8sClient.Get(ctx, types.NamespacedName{Name: "colo-cluster-a-backup"}, bc)
		}, timeout, interval).Should(MatchError(ContainSubstring("not found")),
			"BackupConfig must be deleted after backup block is removed from spec")
	})

	It("does not delete a BackupConfig owned by a different ConfigBundle", func() {
		cbA := &armadav1.ConfigBundle{
			ObjectMeta: metav1.ObjectMeta{Name: "prune-owner-a"},
			Spec: armadav1.ConfigBundleSpec{
				OrbID:      "colo:prune-owner-a",
				Datacenter: "colo",
				KubernetesClusters: []armadav1.KubernetesClusterSpec{{
					OrbID: "colo:cluster-owned-a",
					Backup: &armadav1.ClusterBackupSpec{
						OrbID: "colo:cluster-owned-a-backup",
						Etcd:  &armadav1.EtcdBackupSpec{OrbID: "colo:cluster-owned-a-etcd", Enabled: ptr.To(true)},
					},
				}},
			},
		}
		Expect(k8sClient.Create(ctx, cbA)).To(Succeed())

		bc := &armadav1.BackupConfig{}
		Eventually(func() error {
			return k8sClient.Get(ctx, types.NamespacedName{Name: "colo-cluster-owned-a-backup"}, bc)
		}, timeout, interval).Should(Succeed())

		// A second ConfigBundle with no clusters reconciles. It must not prune cbA's BackupConfig.
		cbB := &armadav1.ConfigBundle{
			ObjectMeta: metav1.ObjectMeta{Name: "prune-owner-b"},
			Spec: armadav1.ConfigBundleSpec{
				OrbID:      "colo:prune-owner-b",
				Datacenter: "colo",
			},
		}
		Expect(k8sClient.Create(ctx, cbB)).To(Succeed())

		// Give the reconciler time to run for cbB.
		Eventually(func(g Gomega) {
			var fresh armadav1.ConfigBundle
			g.Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "prune-owner-b"}, &fresh)).To(Succeed())
			g.Expect(fresh.Status.ObservedGeneration).To(Equal(fresh.Generation))
		}, timeout, interval).Should(Succeed())

		// cbA's BackupConfig must still exist.
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "colo-cluster-owned-a-backup"}, bc)).To(Succeed())
	})

	It("deletes a ServerConfig when its server is removed from the spec", func() {
		cb := &armadav1.ConfigBundle{
			ObjectMeta: metav1.ObjectMeta{Name: "prune-sc-test"},
			Spec: armadav1.ConfigBundleSpec{
				OrbID:      "colo:prune-sc-test",
				Datacenter: "colo",
				Servers: []armadav1.ServerSpec{
					{OrbID: "colo:srv-aaa", ServiceTag: "AAA", Hostname: ptr.To("host-aaa"), OobIP: ptr.To("10.0.0.1")},
					{OrbID: "colo:srv-bbb", ServiceTag: "BBB", Hostname: ptr.To("host-bbb"), OobIP: ptr.To("10.0.0.2")},
				},
			},
		}
		Expect(k8sClient.Create(ctx, cb)).To(Succeed())

		for _, name := range []string{"host-aaa", "host-bbb"} {
			sc := &armadav1.ServerConfig{}
			Eventually(func() error {
				return k8sClient.Get(ctx, types.NamespacedName{Name: name}, sc)
			}, timeout, interval).Should(Succeed(), "ServerConfig %s must be created initially", name)
		}

		// Drop host-bbb from the spec.
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "prune-sc-test"}, cb)).To(Succeed())
		cb.Spec.Servers = cb.Spec.Servers[:1]
		Expect(k8sClient.Update(ctx, cb)).To(Succeed())

		sc := &armadav1.ServerConfig{}
		Eventually(func() error {
			return k8sClient.Get(ctx, types.NamespacedName{Name: "host-bbb"}, sc)
		}, timeout, interval).Should(MatchError(ContainSubstring("not found")),
			"ServerConfig host-bbb must be deleted after server is removed from spec")

		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "host-aaa"}, sc)).To(Succeed(),
			"ServerConfig host-aaa must still exist")
	})
})

// singleServerBundle returns a ConfigBundle with one server entry for use in tests.
// Bundle orbId is derived from name; server orbId is derived from serviceTag.
func singleServerBundle(name, hostname, serviceTag, oobIP string) *armadav1.ConfigBundle {
	return &armadav1.ConfigBundle{
		ObjectMeta: metav1.ObjectMeta{
			Name: name,
		},
		Spec: armadav1.ConfigBundleSpec{
			OrbID:      "colo:" + name,
			Datacenter: "colo",
			Servers: []armadav1.ServerSpec{
				{OrbID: "colo:srv-" + strings.ToLower(serviceTag), ServiceTag: serviceTag, Hostname: ptr.To(hostname), OobIP: ptr.To(oobIP)},
			},
		},
	}
}

// findServerByTag returns the ServerSpec with the given serviceTag, or nil if not found.
func findServerByTag(servers []armadav1.ServerSpec, serviceTag string) *armadav1.ServerSpec {
	for i := range servers {
		if servers[i].ServiceTag == serviceTag {
			return &servers[i]
		}
	}
	return nil
}

// hasAdminEntryFor reports whether local:admin holds a managedFields entry
// pointing at the server with the given orbId (at any field depth).
func hasAdminEntryFor(managedFields []metav1.ManagedFieldsEntry, orbID string) bool {
	wantKey := fmt.Sprintf(`k:{"orbId":%q}`, orbID)
	for _, entry := range managedFields {
		if entry.Manager != "local:admin" || entry.FieldsV1 == nil {
			continue
		}
		var fields map[string]any
		if err := json.Unmarshal(entry.FieldsV1.Raw, &fields); err != nil {
			continue
		}
		specFields, _ := fields["f:spec"].(map[string]any)
		serverFields, _ := specFields["f:servers"].(map[string]any)
		if _, ok := serverFields[wantKey]; ok {
			return true
		}
	}
	return false
}
