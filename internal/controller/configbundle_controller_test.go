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
		Expect(k8sClient.Delete(ctx, &corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{Name: ns},
		})).To(Succeed())
	})

	Describe("child CR decomposition", func() {
		It("creates a ServerConfig named by lowercase hostname", func() {
			cb := singleServerBundle("test-bundle", ns, "colo-r740-01", "3RK3V64", "10.10.1.45")
			Expect(k8sClient.Create(ctx, cb)).To(Succeed())

			sc := &armadav1.ServerConfig{}
			Eventually(func() error {
				return k8sClient.Get(ctx, types.NamespacedName{Name: "colo-r740-01", Namespace: ns}, sc)
			}, timeout, interval).Should(Succeed())

			Expect(sc.Spec.ServiceTag).To(Equal("3RK3V64"))
			Expect(sc.Spec.Hostname).To(Equal(ptr.To("colo-r740-01")))
			Expect(sc.Spec.OobIP).To(Equal(ptr.To("10.10.1.45")))
		})

		It("propagates all idrac fields to the child CR", func() {
			cb := singleServerBundle("test-bundle", ns, "colo-r740-01", "3RK3V64", "10.10.1.45")
			cb.Spec.Servers[0].Idrac = armadav1.IdracSpec{
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
				return k8sClient.Get(ctx, types.NamespacedName{Name: "colo-r740-01", Namespace: ns}, sc)
			}, timeout, interval).Should(Succeed())

			Expect(sc.Spec.Idrac.FirmwareVersion).To(Equal(ptr.To("7.20.10.05")))
			Expect(sc.Spec.Idrac.UsbManagementPortEnabled).To(Equal(ptr.To(true)))
			Expect(sc.Spec.Idrac.RacadmEnabled).To(Equal(ptr.To(true)))
			Expect(sc.Spec.Idrac.SSHEnabled).To(Equal(ptr.To(false)))
			Expect(sc.Spec.Idrac.IPMIEnabled).To(Equal(ptr.To(false)))
			Expect(sc.Spec.Idrac.DHCPEnabled).To(Equal(ptr.To(false)))
			Expect(sc.Spec.Idrac.LockdownModeEnabled).To(Equal(ptr.To(false)))
			Expect(sc.Spec.Idrac.OsToIdracPassThroughEnabled).To(Equal(ptr.To(false)))
		})

		It("creates one ServerConfig per server in a multi-server bundle", func() {
			cb := &armadav1.ConfigBundle{
				ObjectMeta: metav1.ObjectMeta{Name: "multi-galleon", Namespace: ns},
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
					return k8sClient.Get(ctx, types.NamespacedName{Name: hostname, Namespace: ns}, sc)
				}, timeout, interval).Should(Succeed(), "expected ServerConfig %s to exist", hostname)
			}
		})
	})

	Describe("desired state enforcement", func() {
		It("restores a child CR field mutated out-of-band", func() {
			cb := singleServerBundle("test-bundle", ns, "colo-r740-01", "3RK3V64", "10.10.1.45")
			cb.Spec.Servers[0].Idrac.SSHEnabled = ptr.To(false)
			Expect(k8sClient.Create(ctx, cb)).To(Succeed())

			// Wait for the child CR to be created.
			sc := &armadav1.ServerConfig{}
			Eventually(func() error {
				return k8sClient.Get(ctx, types.NamespacedName{Name: "colo-r740-01", Namespace: ns}, sc)
			}, timeout, interval).Should(Succeed())

			// Simulate unauthorized drift: patch sshEnabled to true directly on the child.
			scPatched := sc.DeepCopy()
			scPatched.Spec.Idrac.SSHEnabled = ptr.To(true)
			Expect(k8sClient.Patch(ctx, scPatched, client.MergeFrom(sc))).To(Succeed())

			// The controller (triggered by Owns watch) should restore sshEnabled to false.
			Eventually(func(g Gomega) {
				g.Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "colo-r740-01", Namespace: ns}, sc)).To(Succeed())
				g.Expect(sc.Spec.Idrac.SSHEnabled).To(Equal(ptr.To(false)))
			}, timeout, interval).Should(Succeed())
		})

		It("propagates a ConfigBundle spec update to the child CR", func() {
			cb := singleServerBundle("test-bundle", ns, "colo-r740-01", "3RK3V64", "10.10.1.45")
			cb.Spec.Servers[0].Idrac.SSHEnabled = ptr.To(false)
			Expect(k8sClient.Create(ctx, cb)).To(Succeed())

			// Wait for child CR.
			sc := &armadav1.ServerConfig{}
			Eventually(func() error {
				return k8sClient.Get(ctx, types.NamespacedName{Name: "colo-r740-01", Namespace: ns}, sc)
			}, timeout, interval).Should(Succeed())

			// Update the ConfigBundle spec — desired state changes.
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "test-bundle", Namespace: ns}, cb)).To(Succeed())
			cb.Spec.Servers[0].Idrac.SSHEnabled = ptr.To(true)
			Expect(k8sClient.Update(ctx, cb)).To(Succeed())

			// Child CR must reflect the updated desired state.
			Eventually(func(g Gomega) {
				g.Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "colo-r740-01", Namespace: ns}, sc)).To(Succeed())
				g.Expect(sc.Spec.Idrac.SSHEnabled).To(Equal(ptr.To(true)))
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
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: datacenter, Namespace: ns}, &cb)).To(Succeed())
		Expect(cb.Spec.Datacenter).To(Equal(datacenter))
		Expect(cb.Spec.Servers).To(HaveLen(1))
		Expect(cb.Spec.Servers[0].ServiceTag).To(Equal("3RK3V64"))
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
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: datacenter, Namespace: ns}, &cb)).To(Succeed())
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
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: datacenter, Namespace: ns}, &cb)).To(Succeed())
		Expect(cb.Spec.Servers).To(HaveLen(2))
		Expect(cb.Status.LastAppliedDigest).To(Equal("sha256:v2"))
		Expect(cb.Status.LastOrbImportID).To(Equal("import-2"))
	})

	It("omits only admin-owned leaves; controller still updates the rest of the same server", func() {
		const datacenter = "colo"

		// Step 1: controller seeds the CR with server A's full state.
		seed := testManifest(datacenter,
			armadav1.ServerSpec{OrbID: "colo:srv-aaa0001", ServiceTag: "AAA0001", Hostname: ptr.To("colo-r740-01"), OobIP: ptr.To("10.10.1.45"),
				Idrac: armadav1.IdracSpec{SSHEnabled: ptr.To(false), FirmwareVersion: ptr.To("7.0.0"), RacadmEnabled: ptr.To(false)}},
		)
		Expect(server.applyManifest(ctx, seed, "sha256:seed", "import-0")).To(Succeed())

		// Step 2: local:admin overrides ONE leaf — idrac.sshEnabled — on server A.
		// Build the apply as unstructured so we claim ONLY the leaf we set; struct
		// marshaling would serialize zero-valued primitives and claim too much.
		adminApply := &unstructured.Unstructured{Object: map[string]any{
			"apiVersion": armadav1.GroupVersion.String(),
			"kind":       "ConfigBundle",
			"metadata":   map[string]any{"name": datacenter, "namespace": ns},
			"spec": map[string]any{
				"servers": []any{
					map[string]any{
						"orbId": "colo:srv-aaa0001",
						"idrac": map[string]any{"sshEnabled": true},
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
				Idrac: armadav1.IdracSpec{SSHEnabled: ptr.To(false), FirmwareVersion: ptr.To("7.20.10.05"), RacadmEnabled: ptr.To(true)}},
			armadav1.ServerSpec{OrbID: "colo:srv-bbb0002", ServiceTag: "BBB0002", Hostname: ptr.To("colo-r740-02"), OobIP: ptr.To("10.10.1.46")},
		)
		Expect(server.applyManifest(ctx, body, "sha256:newdigest", "import-1")).To(Succeed())

		var cb armadav1.ConfigBundle
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: datacenter, Namespace: ns}, &cb)).To(Succeed())

		serverB := findServerByTag(cb.Spec.Servers, "BBB0002")
		Expect(serverB).NotTo(BeNil(), "server B must be present")

		serverA := findServerByTag(cb.Spec.Servers, "AAA0001")
		Expect(serverA).NotTo(BeNil(), "server A must still be present")

		// Admin-owned leaf preserved:
		Expect(serverA.Idrac.SSHEnabled).To(Equal(ptr.To(true)), "admin's sshEnabled override must be preserved")

		// Controller-updatable leaves propagated:
		Expect(serverA.OobIP).To(Equal(ptr.To("10.10.1.99")), "controller's oobIP update must take effect")
		Expect(serverA.Idrac.FirmwareVersion).To(Equal(ptr.To("7.20.10.05")), "controller's firmwareVersion update must take effect")
		Expect(serverA.Idrac.RacadmEnabled).To(Equal(ptr.To(true)), "controller's racadmEnabled update must take effect")
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
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: datacenter, Namespace: ns}, &cb)).To(Succeed())
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
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: datacenter, Namespace: ns}, &final)).To(Succeed())
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
			ObjectMeta: metav1.ObjectMeta{Name: "ssa-isolation-test", Namespace: ns},
			Spec: armadav1.ConfigBundleSpec{
				OrbID:      "colo:colo",
				Datacenter: "colo",
				Servers: []armadav1.ServerSpec{
					{
						OrbID:      "colo:srv-aaa0001",
						ServiceTag: "AAA0001",
						Hostname:   ptr.To("colo-r740-01"),
						OobIP:      ptr.To("10.10.1.45"),
						Idrac:      armadav1.IdracSpec{SSHEnabled: ptr.To(true)},
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
			ObjectMeta: metav1.ObjectMeta{Name: "ssa-isolation-test", Namespace: ns},
			Spec: armadav1.ConfigBundleSpec{
				OrbID:      "colo:colo",
				Datacenter: "colo",
				Servers: []armadav1.ServerSpec{
					{
						OrbID:      "colo:srv-bbb0002",
						ServiceTag: "BBB0002",
						Hostname:   ptr.To("colo-r740-02"),
						OobIP:      ptr.To("10.10.1.46"),
						Idrac:      armadav1.IdracSpec{SSHEnabled: ptr.To(false)},
					},
				},
			},
		}
		Expect(k8sClient.Patch(ctx, pullerApply, client.Apply,
			client.FieldOwner("configbundle-controller"),
		)).To(Succeed(), "+listType=map must scope ownership to individual entries — Puller apply of server B must not 409")

		result := &armadav1.ConfigBundle{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "ssa-isolation-test", Namespace: ns}, result)).To(Succeed())

		serverA := findServerByTag(result.Spec.Servers, "AAA0001")
		Expect(serverA).NotTo(BeNil(), "server A (admin-owned) must still be present")
		Expect(serverA.Idrac.SSHEnabled).To(Equal(ptr.To(true)), "admin override on server A must be preserved")

		serverB := findServerByTag(result.Spec.Servers, "BBB0002")
		Expect(serverB).NotTo(BeNil(), "server B (Puller-owned) must be present")
		Expect(serverB.Idrac.SSHEnabled).To(Equal(ptr.To(false)), "Puller's desired state for server B must land")
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
		Expect(k8sClient.Delete(ctx, &corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{Name: ns},
		})).To(Succeed())
	})

	It("detects a local:admin override and produces correct override entries", func() {
		const datacenter = "colo"

		// Step 1: configbundle-controller applies the initial spec (cloud intent).
		controllerApply := &armadav1.ConfigBundle{
			TypeMeta:   metav1.TypeMeta{APIVersion: armadav1.GroupVersion.String(), Kind: "ConfigBundle"},
			ObjectMeta: metav1.ObjectMeta{Name: datacenter, Namespace: ns},
			Spec: armadav1.ConfigBundleSpec{
				OrbID:      "colo:colo",
				Datacenter: datacenter,
				Servers: []armadav1.ServerSpec{
					{
						OrbID:      "colo:srv-3rk3v64",
						ServiceTag: "3RK3V64",
						Hostname:   ptr.To("colo-r740-01"),
						OobIP:      ptr.To("10.10.1.45"),
						Idrac:      armadav1.IdracSpec{SSHEnabled: ptr.To(false), RacadmEnabled: ptr.To(true)},
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
			"metadata":   map[string]any{"name": datacenter, "namespace": ns},
			"spec": map[string]any{
				"servers": []any{
					map[string]any{
						"orbId": "colo:srv-3rk3v64",
						"idrac": map[string]any{"sshEnabled": true},
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
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: datacenter, Namespace: ns}, &cb)).To(Succeed())

		// Step 4: Build a reporter with a mapping and set the last manifest.
		mapping := envtestMapping(GinkgoT())
		reporter := NewDivergenceReporter(k8sClient,
			WithDivergenceNamespace(ns),
			WithDivergenceEnabled(true),
		)
		reporter.SetLastManifest(datacenter, controllerApply.Spec)

		// Step 5: Extract overrides and verify orbital-native fields.
		overrides := reporter.extractOverrides(&cb, mapping, controllerApply.Spec)
		Expect(overrides).NotTo(BeEmpty(), "should detect at least one override")

		var found *OverrideEntry
		for i := range overrides {
			if overrides[i].OrbID == "colo:srv-001-idrac" && overrides[i].Field == "sshEnabled" {
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
			ObjectMeta: metav1.ObjectMeta{Name: datacenter, Namespace: ns},
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
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: datacenter, Namespace: ns}, &cb)).To(Succeed())

		mapping := envtestMapping(GinkgoT())
		reporter := NewDivergenceReporter(k8sClient,
			WithDivergenceNamespace(ns),
			WithDivergenceEnabled(true),
		)
		reporter.SetLastManifest(datacenter, controllerApply.Spec)

		overrides := reporter.extractOverrides(&cb, mapping, controllerApply.Spec)
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
			ObjectMeta: metav1.ObjectMeta{Name: datacenter, Namespace: ns},
			Spec: armadav1.ConfigBundleSpec{
				OrbID:      "colo:colo",
				Datacenter: datacenter,
				Servers: []armadav1.ServerSpec{
					{
						OrbID:      "colo:srv-3rk3v64",
						ServiceTag: "3RK3V64",
						Hostname:   ptr.To("colo-r740-01"),
						OobIP:      ptr.To("10.10.1.45"),
						Idrac:      armadav1.IdracSpec{SSHEnabled: ptr.To(false)},
					},
				},
			},
		}
		Expect(k8sClient.Patch(ctx, controllerApply, client.Apply,
			client.FieldOwner("configbundle-controller"),
		)).To(Succeed())

		// Step 2: local:admin overrides sshEnabled.
		// Build as unstructured so we claim only the leaf we set.
		adminApply := &unstructured.Unstructured{Object: map[string]any{
			"apiVersion": armadav1.GroupVersion.String(),
			"kind":       "ConfigBundle",
			"metadata":   map[string]any{"name": datacenter, "namespace": ns},
			"spec": map[string]any{
				"servers": []any{
					map[string]any{
						"orbId": "colo:srv-3rk3v64",
						"idrac": map[string]any{"sshEnabled": true},
					},
				},
			},
		}}
		Expect(k8sClient.Patch(ctx, adminApply, client.Apply,
			client.FieldOwner("local:admin"),
			client.ForceOwnership,
		)).To(Succeed())

		// Update status with a digest so the reporter can find the ConfigBundle.
		var cb armadav1.ConfigBundle
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: datacenter, Namespace: ns}, &cb)).To(Succeed())
		cb.Status.LastAppliedDigest = "sha256:test-digest"
		Expect(k8sClient.Status().Update(ctx, &cb)).To(Succeed())

		// Step 3: Write the mapping ConfigMap (simulating what handleMappingBody does).
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: datacenter, Namespace: ns}, &cb)).To(Succeed())
		mappingJSON, err := json.Marshal(envtestMapping(GinkgoT()))
		Expect(err).NotTo(HaveOccurred())
		ownerRef := metav1.OwnerReference{
			APIVersion:         armadav1.GroupVersion.String(),
			Kind:               "ConfigBundle",
			Name:               cb.Name,
			UID:                cb.UID,
			Controller:         ptr.To(true),
			BlockOwnerDeletion: ptr.To(true),
		}
		Expect(writeMappingConfigMap(ctx, k8sClient, ns, datacenter, "sha256:test-digest", mappingJSON, ownerRef)).To(Succeed())

		// Step 4: Run reporter.Reconcile() directly (lastEventAt is zero → startup case, no debounce).
		reporter := NewDivergenceReporter(k8sClient,
			WithDivergenceNamespace(ns),
			WithDivergenceEnabled(true),
			WithDivergenceIntakeURL(intake.URL),
		)
		reporter.SetLastManifest(datacenter, controllerApply.Spec)

		_, err = reporter.Reconcile(ctx, reconcile.Request{
			NamespacedName: types.NamespacedName{Name: datacenter, Namespace: ns},
		})
		Expect(err).NotTo(HaveOccurred())

		// Step 5: Verify the captured payload — orbital-native, no bundleDigest.
		Expect(capturedPayload.Overrides).NotTo(BeEmpty())

		var sshOverride *OverrideEntry
		for i := range capturedPayload.Overrides {
			if capturedPayload.Overrides[i].OrbID == "colo:srv-001-idrac" && capturedPayload.Overrides[i].Field == "sshEnabled" {
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
// Mapping ConfigMap envtest tests
// ---------------------------------------------------------------------------

var _ = Describe("DispatchServer mapping via ConfigMap", func() {
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
		ns = fmt.Sprintf("mapping-cm-%d", nsCounter)
		Expect(k8sClient.Create(ctx, &corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{Name: ns},
		})).To(Succeed())
		server = NewConsumeServer(k8sClient,
			WithNamespace(ns),
			WithRetry(1, 0),
		)
	})

	AfterEach(func() {
		Expect(k8sClient.Delete(ctx, &corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{Name: ns},
		})).To(Succeed())
	})

	It("stores mapping in a ConfigMap with OwnerReference on dispatch", func() {
		const datacenter = "colo"
		body := testManifest(datacenter,
			armadav1.ServerSpec{OrbID: "colo:srv-3rk3v64", ServiceTag: "3RK3V64", Hostname: ptr.To("colo-r740-01"), OobIP: ptr.To("10.10.1.45")},
		)

		// Apply manifest to create the CR.
		Expect(server.applyManifest(ctx, body, "sha256:v1", "import-1")).To(Succeed())

		// Fetch CR to get UID.
		var cb armadav1.ConfigBundle
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: datacenter, Namespace: ns}, &cb)).To(Succeed())

		// Write mapping ConfigMap with OwnerReference (simulating handleMappingBody).
		mappingRaw, err := json.Marshal(envtestMapping(GinkgoT()))
		Expect(err).NotTo(HaveOccurred())
		ownerRef := metav1.OwnerReference{
			APIVersion:         armadav1.GroupVersion.String(),
			Kind:               "ConfigBundle",
			Name:               cb.Name,
			UID:                cb.UID,
			Controller:         ptr.To(true),
			BlockOwnerDeletion: ptr.To(true),
		}
		Expect(writeMappingConfigMap(ctx, k8sClient, ns, datacenter, "sha256:v1", mappingRaw, ownerRef)).To(Succeed())

		// Verify ConfigMap exists with correct labels and data.
		var cm corev1.ConfigMap
		Expect(k8sClient.Get(ctx, types.NamespacedName{
			Name:      MappingConfigMapName(datacenter),
			Namespace: ns,
		}, &cm)).To(Succeed())

		Expect(cm.Labels).To(HaveKeyWithValue("armada.ai/configbundle", datacenter))
		Expect(cm.Labels).To(HaveKeyWithValue("armada.ai/component", "mapping"))
		Expect(cm.Data).To(HaveKey("digest"))
		Expect(cm.Data["digest"]).To(Equal("sha256:v1"))
		Expect(cm.Data).To(HaveKey("mapping.json"))
		Expect(cm.OwnerReferences).To(HaveLen(1))
		Expect(cm.OwnerReferences[0].Name).To(Equal(datacenter))
		Expect(cm.OwnerReferences[0].UID).To(Equal(cb.UID))

		// Read it back with readMappingConfigMap and verify parse.
		m, err := readMappingConfigMap(ctx, k8sClient, ns, datacenter)
		Expect(err).NotTo(HaveOccurred())
		Expect(m.Items).NotTo(BeEmpty())
	})

	It("sets a blocking OwnerReference on the ConfigMap pointing to the ConfigBundle CR", func() {
		const datacenter = "colo"
		body := testManifest(datacenter,
			armadav1.ServerSpec{OrbID: "colo:srv-3rk3v64", ServiceTag: "3RK3V64", Hostname: ptr.To("colo-r740-01"), OobIP: ptr.To("10.10.1.45")},
		)

		// Apply manifest to create the CR.
		Expect(server.applyManifest(ctx, body, "sha256:v1", "import-1")).To(Succeed())

		var cb armadav1.ConfigBundle
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: datacenter, Namespace: ns}, &cb)).To(Succeed())

		// Write mapping ConfigMap with OwnerReference.
		mappingRaw, err := json.Marshal(envtestMapping(GinkgoT()))
		Expect(err).NotTo(HaveOccurred())
		ownerRef := metav1.OwnerReference{
			APIVersion:         armadav1.GroupVersion.String(),
			Kind:               "ConfigBundle",
			Name:               cb.Name,
			UID:                cb.UID,
			Controller:         ptr.To(true),
			BlockOwnerDeletion: ptr.To(true),
		}
		Expect(writeMappingConfigMap(ctx, k8sClient, ns, datacenter, "sha256:v1", mappingRaw, ownerRef)).To(Succeed())

		// Verify OwnerReference is set correctly.
		// In a real cluster the GC would delete the ConfigMap when the CR is deleted.
		// Envtest does not run the garbage collector, so we verify the OwnerReference
		// is set with Controller=true and BlockOwnerDeletion=true which is sufficient
		// to enable cascading deletion in production.
		var cm corev1.ConfigMap
		Expect(k8sClient.Get(ctx, types.NamespacedName{
			Name:      MappingConfigMapName(datacenter),
			Namespace: ns,
		}, &cm)).To(Succeed())

		Expect(cm.OwnerReferences).To(HaveLen(1))
		or := cm.OwnerReferences[0]
		Expect(or.Kind).To(Equal("ConfigBundle"))
		Expect(or.Name).To(Equal(datacenter))
		Expect(or.UID).To(Equal(cb.UID))
		Expect(or.Controller).To(Equal(ptr.To(true)))
		Expect(or.BlockOwnerDeletion).To(Equal(ptr.To(true)))
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
		Expect(k8sClient.Delete(ctx, &corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{Name: ns},
		})).To(Succeed())
	})

	It("reclaims a local:admin-owned field and leaves other admin fields intact", func() {
		const datacenter = "colo"

		// Step 1: configbundle-controller applies the initial spec.
		controllerApply := &armadav1.ConfigBundle{
			TypeMeta:   metav1.TypeMeta{APIVersion: armadav1.GroupVersion.String(), Kind: "ConfigBundle"},
			ObjectMeta: metav1.ObjectMeta{Name: datacenter, Namespace: ns},
			Spec: armadav1.ConfigBundleSpec{
				OrbID:      "colo:colo",
				Datacenter: datacenter,
				Servers: []armadav1.ServerSpec{
					{
						OrbID:      "colo:srv-3rk3v64",
						ServiceTag: "3RK3V64",
						Hostname:   ptr.To("colo-r740-01"),
						OobIP:      ptr.To("10.10.1.45"),
						Idrac: armadav1.IdracSpec{
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
			"metadata":   map[string]any{"name": datacenter, "namespace": ns},
			"spec": map[string]any{
				"servers": []any{
					map[string]any{
						"orbId": "colo:srv-3rk3v64",
						"idrac": map[string]any{
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
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: datacenter, Namespace: ns}, &cbBefore)).To(Succeed())
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
					Idrac: armadav1.IdracSpec{
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
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: datacenter, Namespace: ns}, &cbForPatch)).To(Succeed())
		patchSpec, err := omitAdminOwnedFields(spec, cbForPatch.ManagedFields)
		Expect(err).NotTo(HaveOccurred())
		Expect(server.processTakeover(ctx, spec, patchSpec)).To(Succeed())

		// Step 4: Read the CR back and verify field values.
		var cbAfter armadav1.ConfigBundle
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: datacenter, Namespace: ns}, &cbAfter)).To(Succeed())

		srv := findServerByTag(cbAfter.Spec.Servers, "3RK3V64")
		Expect(srv).NotTo(BeNil())

		// sshEnabled should be reclaimed to controller's value (false).
		Expect(srv.Idrac.SSHEnabled).To(Equal(ptr.To(false)), "sshEnabled should be reclaimed to controller intent (false)")

		// racadmEnabled should still be the admin's override value (false).
		Expect(srv.Idrac.RacadmEnabled).To(Equal(ptr.To(false)), "racadmEnabled should still be admin's override value")

		// Verify managedFields: sshEnabled should now be owned by configbundle-controller.
		// local:admin should still own racadmEnabled but NOT sshEnabled.
		adminPaths := extractAdminPaths(cbAfter.ManagedFields)
		var adminOwnsSSH, adminOwnsRacadm bool
		for _, ap := range adminPaths {
			if ap.path == "spec.servers[orbId=colo:srv-3rk3v64].idrac.sshEnabled" {
				adminOwnsSSH = true
			}
			if ap.path == "spec.servers[orbId=colo:srv-3rk3v64].idrac.racadmEnabled" {
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
			ObjectMeta: metav1.ObjectMeta{Name: datacenter, Namespace: ns},
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

// envtestMapping returns a Mapping matching the server entries used in envtests.
func envtestMapping(t interface {
	Helper()
	Fatalf(string, ...interface{})
}) *Mapping {
	t.Helper()
	m, err := ParseMapping([]byte(`{
		"bundleDigest": "sha256:test-digest",
		"items": [
			{"path": "spec", "orbId": "colo:colo-galleon", "type": "DataCenter"},
			{"path": "spec.servers[orbId=colo:srv-3rk3v64]", "orbId": "colo:srv-001", "type": "Server"},
			{"path": "spec.servers[orbId=colo:srv-3rk3v64].idrac", "orbId": "colo:srv-001-idrac", "type": "IdracSettings"}
		]
	}`))
	if err != nil {
		t.Fatalf("ParseMapping: %v", err)
	}
	return m
}

// singleServerBundle returns a ConfigBundle with one server entry for use in tests.
// Bundle orbId is derived from name; server orbId is derived from serviceTag.
func singleServerBundle(name, ns, hostname, serviceTag, oobIP string) *armadav1.ConfigBundle {
	return &armadav1.ConfigBundle{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: ns,
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
