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
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

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
			Expect(sc.Spec.Hostname).To(Equal("colo-r740-01"))
			Expect(sc.Spec.OobIP).To(Equal("10.10.1.45"))
		})

		It("propagates all idrac fields to the child CR", func() {
			cb := singleServerBundle("test-bundle", ns, "colo-r740-01", "3RK3V64", "10.10.1.45")
			cb.Spec.Servers[0].Idrac = armadav1.IdracSpec{
				FirmwareVersion:             "7.20.10.05",
				SSHEnabled:                  false,
				IPMIEnabled:                 false,
				LockdownModeEnabled:         false,
				OsToIdracPassThroughEnabled: false,
				UsbManagementPortEnabled:    true,
				DHCPEnabled:                 false,
				RacadmEnabled:               true,
			}
			Expect(k8sClient.Create(ctx, cb)).To(Succeed())

			sc := &armadav1.ServerConfig{}
			Eventually(func() error {
				return k8sClient.Get(ctx, types.NamespacedName{Name: "colo-r740-01", Namespace: ns}, sc)
			}, timeout, interval).Should(Succeed())

			Expect(sc.Spec.Idrac.FirmwareVersion).To(Equal("7.20.10.05"))
			Expect(sc.Spec.Idrac.UsbManagementPortEnabled).To(BeTrue())
			Expect(sc.Spec.Idrac.RacadmEnabled).To(BeTrue())
			Expect(sc.Spec.Idrac.SSHEnabled).To(BeFalse())
			Expect(sc.Spec.Idrac.IPMIEnabled).To(BeFalse())
			Expect(sc.Spec.Idrac.DHCPEnabled).To(BeFalse())
			Expect(sc.Spec.Idrac.LockdownModeEnabled).To(BeFalse())
			Expect(sc.Spec.Idrac.OsToIdracPassThroughEnabled).To(BeFalse())
		})

		It("creates one ServerConfig per server in a multi-server bundle", func() {
			cb := &armadav1.ConfigBundle{
				ObjectMeta: metav1.ObjectMeta{Name: "multi-galleon", Namespace: ns},
				Spec: armadav1.ConfigBundleSpec{
					Datacenter: "colo",
					Servers: []armadav1.ServerSpec{
						{ServiceTag: "3RK3V64", Hostname: "colo-r740-01", OobIP: "10.10.1.45"},
						{ServiceTag: "FQK3V64", Hostname: "colo-r740-02", OobIP: "10.10.1.46"},
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
			cb.Spec.Servers[0].Idrac.SSHEnabled = false
			Expect(k8sClient.Create(ctx, cb)).To(Succeed())

			// Wait for the child CR to be created.
			sc := &armadav1.ServerConfig{}
			Eventually(func() error {
				return k8sClient.Get(ctx, types.NamespacedName{Name: "colo-r740-01", Namespace: ns}, sc)
			}, timeout, interval).Should(Succeed())

			// Simulate unauthorized drift: patch sshEnabled to true directly on the child.
			scPatched := sc.DeepCopy()
			scPatched.Spec.Idrac.SSHEnabled = true
			Expect(k8sClient.Patch(ctx, scPatched, client.MergeFrom(sc))).To(Succeed())

			// The controller (triggered by Owns watch) should restore sshEnabled to false.
			Eventually(func(g Gomega) {
				g.Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "colo-r740-01", Namespace: ns}, sc)).To(Succeed())
				g.Expect(sc.Spec.Idrac.SSHEnabled).To(BeFalse())
			}, timeout, interval).Should(Succeed())
		})

		It("propagates a ConfigBundle spec update to the child CR", func() {
			cb := singleServerBundle("test-bundle", ns, "colo-r740-01", "3RK3V64", "10.10.1.45")
			cb.Spec.Servers[0].Idrac.SSHEnabled = false
			Expect(k8sClient.Create(ctx, cb)).To(Succeed())

			// Wait for child CR.
			sc := &armadav1.ServerConfig{}
			Eventually(func() error {
				return k8sClient.Get(ctx, types.NamespacedName{Name: "colo-r740-01", Namespace: ns}, sc)
			}, timeout, interval).Should(Succeed())

			// Update the ConfigBundle spec — desired state changes.
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "test-bundle", Namespace: ns}, cb)).To(Succeed())
			cb.Spec.Servers[0].Idrac.SSHEnabled = true
			Expect(k8sClient.Update(ctx, cb)).To(Succeed())

			// Child CR must reflect the updated desired state.
			Eventually(func(g Gomega) {
				g.Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "colo-r740-01", Namespace: ns}, sc)).To(Succeed())
				g.Expect(sc.Spec.Idrac.SSHEnabled).To(BeTrue())
			}, timeout, interval).Should(Succeed())
		})
	})
})

// testManifest builds a minimal ConfigBundle manifest YAML for use in ConsumeServer tests.
func testManifest(datacenter string, servers ...armadav1.ServerSpec) []byte {
	y := fmt.Sprintf("datacenter: %s\n", datacenter)
	if len(servers) > 0 {
		y += "servers:\n"
		for _, s := range servers {
			y += fmt.Sprintf("  - serviceTag: %q\n    hostname: %q\n    oobIP: %q\n",
				s.ServiceTag, s.Hostname, s.OobIP)
		}
	}
	return []byte(y)
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
			armadav1.ServerSpec{ServiceTag: "3RK3V64", Hostname: "colo-r740-01", OobIP: "10.10.1.45"},
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
			armadav1.ServerSpec{ServiceTag: "3RK3V64", Hostname: "colo-r740-01", OobIP: "10.10.1.45"},
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
			armadav1.ServerSpec{ServiceTag: "3RK3V64", Hostname: "r1", OobIP: "10.0.0.1"},
		), "sha256:v1", "import-1")).To(Succeed())

		Expect(server.applyManifest(ctx, testManifest(datacenter,
			armadav1.ServerSpec{ServiceTag: "3RK3V64", Hostname: "r1", OobIP: "10.0.0.1"},
			armadav1.ServerSpec{ServiceTag: "FQK3V64", Hostname: "r2", OobIP: "10.0.0.2"},
		), "sha256:v2", "import-2")).To(Succeed())

		var cb armadav1.ConfigBundle
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: datacenter, Namespace: ns}, &cb)).To(Succeed())
		Expect(cb.Spec.Servers).To(HaveLen(2))
		Expect(cb.Status.LastAppliedDigest).To(Equal("sha256:v2"))
		Expect(cb.Status.LastOrbImportID).To(Equal("import-2"))
	})

	It("omits local:admin-owned server entries from the SSA patch", func() {
		const datacenter = "colo"

		// Step 1: local:admin claims server A.
		adminApply := &armadav1.ConfigBundle{
			TypeMeta:   metav1.TypeMeta{APIVersion: armadav1.GroupVersion.String(), Kind: "ConfigBundle"},
			ObjectMeta: metav1.ObjectMeta{Name: datacenter, Namespace: ns},
			Spec: armadav1.ConfigBundleSpec{
				Datacenter: datacenter,
				Servers: []armadav1.ServerSpec{
					{ServiceTag: "AAA0001", Hostname: "colo-r740-01", OobIP: "10.10.1.45",
						Idrac: armadav1.IdracSpec{SSHEnabled: true}},
				},
			},
		}
		Expect(k8sClient.Patch(ctx, adminApply, client.Apply,
			client.FieldOwner("local:admin"),
		)).To(Succeed())

		// Step 2: dispatch includes both server A (admin-owned) and server B (uncontested).
		// applyManifest must omit A and apply B without 409.
		body := testManifest(datacenter,
			armadav1.ServerSpec{ServiceTag: "AAA0001", Hostname: "colo-r740-01", OobIP: "10.10.1.45"},
			armadav1.ServerSpec{ServiceTag: "BBB0002", Hostname: "colo-r740-02", OobIP: "10.10.1.46"},
		)
		Expect(server.applyManifest(ctx, body, "sha256:newdigest", "import-1")).To(Succeed())

		var cb armadav1.ConfigBundle
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: datacenter, Namespace: ns}, &cb)).To(Succeed())
		serverB := findServerByTag(cb.Spec.Servers, "BBB0002")
		Expect(serverB).NotTo(BeNil(), "server B must be present")
		serverA := findServerByTag(cb.Spec.Servers, "AAA0001")
		Expect(serverA).NotTo(BeNil(), "server A must still be present (SSA preserves admin entry)")
		Expect(serverA.Idrac.SSHEnabled).To(BeTrue(), "admin override must be preserved")
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
		// With +listType=map, ownership is scoped to the individual entry by serviceTag.
		// Without the annotation the entire servers[] array would be owned atomically by admin,
		// and step 2 below would return 409 even though it only touches server B.
		adminApply := &armadav1.ConfigBundle{
			TypeMeta: metav1.TypeMeta{
				APIVersion: armadav1.GroupVersion.String(),
				Kind:       "ConfigBundle",
			},
			ObjectMeta: metav1.ObjectMeta{Name: "ssa-isolation-test", Namespace: ns},
			Spec: armadav1.ConfigBundleSpec{
				Datacenter: "colo",
				Servers: []armadav1.ServerSpec{
					{
						ServiceTag: "AAA0001",
						Hostname:   "colo-r740-01",
						OobIP:      "10.10.1.45",
						Idrac:      armadav1.IdracSpec{SSHEnabled: true},
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
				Datacenter: "colo",
				Servers: []armadav1.ServerSpec{
					{
						ServiceTag: "BBB0002",
						Hostname:   "colo-r740-02",
						OobIP:      "10.10.1.46",
						Idrac:      armadav1.IdracSpec{SSHEnabled: false},
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
		Expect(serverA.Idrac.SSHEnabled).To(BeTrue(), "admin override on server A must be preserved")

		serverB := findServerByTag(result.Spec.Servers, "BBB0002")
		Expect(serverB).NotTo(BeNil(), "server B (Puller-owned) must be present")
		Expect(serverB.Idrac.SSHEnabled).To(BeFalse(), "Puller's desired state for server B must land")
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
				Datacenter: datacenter,
				Servers: []armadav1.ServerSpec{
					{
						ServiceTag: "3RK3V64",
						Hostname:   "colo-r740-01",
						OobIP:      "10.10.1.45",
						Idrac:      armadav1.IdracSpec{SSHEnabled: false, RacadmEnabled: true},
					},
				},
			},
		}
		Expect(k8sClient.Patch(ctx, controllerApply, client.Apply,
			client.FieldOwner("configbundle-controller"),
		)).To(Succeed())

		// Step 2: local:admin overrides sshEnabled to true.
		adminApply := &armadav1.ConfigBundle{
			TypeMeta:   metav1.TypeMeta{APIVersion: armadav1.GroupVersion.String(), Kind: "ConfigBundle"},
			ObjectMeta: metav1.ObjectMeta{Name: datacenter, Namespace: ns},
			Spec: armadav1.ConfigBundleSpec{
				Datacenter: datacenter,
				Servers: []armadav1.ServerSpec{
					{
						ServiceTag: "3RK3V64",
						Idrac:      armadav1.IdracSpec{SSHEnabled: true},
					},
				},
			},
		}
		Expect(k8sClient.Patch(ctx, adminApply, client.Apply,
			client.FieldOwner("local:admin"),
			client.ForceOwnership,
		)).To(Succeed())

		// Step 3: Read the CR back to get managedFields.
		var cb armadav1.ConfigBundle
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: datacenter, Namespace: ns}, &cb)).To(Succeed())

		// Step 4: Build a reporter and set the last manifest (cloud intent).
		reporter := NewDivergenceReporter(k8sClient,
			WithDivergenceNamespace(ns),
			WithDivergenceEnabled(true),
		)
		reporter.SetLastManifest(datacenter, controllerApply.Spec)

		// Step 5: Extract overrides and verify.
		overrides := reporter.extractOverrides(&cb)
		Expect(overrides).NotTo(BeEmpty(), "should detect at least one override")

		// Find the sshEnabled override.
		var found *OverrideEntry
		for i := range overrides {
			if overrides[i].Path == "spec.servers[serviceTag=3RK3V64].idrac.sshEnabled" {
				found = &overrides[i]
				break
			}
		}
		Expect(found).NotTo(BeNil(), "should find sshEnabled override")
		Expect(found.OverrideValue).To(Equal(true), "override value should be true")
		Expect(found.IntendedValue).To(Equal(false), "intended value should be false")
		Expect(found.Who).To(Equal("local:admin"))
		Expect(found.When).NotTo(BeEmpty())
	})

	It("reports empty overrides when no local:admin fields exist", func() {
		const datacenter = "colo"

		controllerApply := &armadav1.ConfigBundle{
			TypeMeta:   metav1.TypeMeta{APIVersion: armadav1.GroupVersion.String(), Kind: "ConfigBundle"},
			ObjectMeta: metav1.ObjectMeta{Name: datacenter, Namespace: ns},
			Spec: armadav1.ConfigBundleSpec{
				Datacenter: datacenter,
				Servers: []armadav1.ServerSpec{
					{ServiceTag: "3RK3V64", Hostname: "colo-r740-01", OobIP: "10.10.1.45"},
				},
			},
		}
		Expect(k8sClient.Patch(ctx, controllerApply, client.Apply,
			client.FieldOwner("configbundle-controller"),
		)).To(Succeed())

		var cb armadav1.ConfigBundle
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: datacenter, Namespace: ns}, &cb)).To(Succeed())

		reporter := NewDivergenceReporter(k8sClient,
			WithDivergenceNamespace(ns),
			WithDivergenceEnabled(true),
		)
		reporter.SetLastManifest(datacenter, controllerApply.Spec)

		overrides := reporter.extractOverrides(&cb)
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
				Datacenter: datacenter,
				Servers: []armadav1.ServerSpec{
					{
						ServiceTag: "3RK3V64",
						Hostname:   "colo-r740-01",
						OobIP:      "10.10.1.45",
						Idrac:      armadav1.IdracSpec{SSHEnabled: false},
					},
				},
			},
		}
		Expect(k8sClient.Patch(ctx, controllerApply, client.Apply,
			client.FieldOwner("configbundle-controller"),
		)).To(Succeed())

		// Step 2: local:admin overrides sshEnabled.
		adminApply := &armadav1.ConfigBundle{
			TypeMeta:   metav1.TypeMeta{APIVersion: armadav1.GroupVersion.String(), Kind: "ConfigBundle"},
			ObjectMeta: metav1.ObjectMeta{Name: datacenter, Namespace: ns},
			Spec: armadav1.ConfigBundleSpec{
				Datacenter: datacenter,
				Servers: []armadav1.ServerSpec{
					{ServiceTag: "3RK3V64", Idrac: armadav1.IdracSpec{SSHEnabled: true}},
				},
			},
		}
		Expect(k8sClient.Patch(ctx, adminApply, client.Apply,
			client.FieldOwner("local:admin"),
			client.ForceOwnership,
		)).To(Succeed())

		// Update status with a digest so the payload includes it.
		var cb armadav1.ConfigBundle
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: datacenter, Namespace: ns}, &cb)).To(Succeed())
		cb.Status.LastAppliedDigest = "sha256:test-digest"
		Expect(k8sClient.Status().Update(ctx, &cb)).To(Succeed())

		// Step 3: Run reporter.report().
		reporter := NewDivergenceReporter(k8sClient,
			WithDivergenceNamespace(ns),
			WithDivergenceEnabled(true),
			WithDivergenceIntakeURL(intake.URL),
		)
		reporter.SetLastManifest(datacenter, controllerApply.Spec)

		Expect(reporter.report(ctx)).To(Succeed())

		// Step 4: Verify the captured payload.
		Expect(capturedPayload.BundleDigest).To(Equal("sha256:test-digest"))
		Expect(capturedPayload.Overrides).NotTo(BeEmpty())

		var sshOverride *OverrideEntry
		for i := range capturedPayload.Overrides {
			if capturedPayload.Overrides[i].Path == "spec.servers[serviceTag=3RK3V64].idrac.sshEnabled" {
				sshOverride = &capturedPayload.Overrides[i]
				break
			}
		}
		Expect(sshOverride).NotTo(BeNil())
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
				Datacenter: datacenter,
				Servers: []armadav1.ServerSpec{
					{
						ServiceTag: "3RK3V64",
						Hostname:   "colo-r740-01",
						OobIP:      "10.10.1.45",
						Idrac: armadav1.IdracSpec{
							SSHEnabled:    false,
							RacadmEnabled: true,
						},
					},
				},
			},
		}
		Expect(k8sClient.Patch(ctx, controllerApply, client.Apply,
			client.FieldOwner("configbundle-controller"),
		)).To(Succeed())

		// Step 2: local:admin overrides both sshEnabled AND racadmEnabled.
		adminApply := &armadav1.ConfigBundle{
			TypeMeta:   metav1.TypeMeta{APIVersion: armadav1.GroupVersion.String(), Kind: "ConfigBundle"},
			ObjectMeta: metav1.ObjectMeta{Name: datacenter, Namespace: ns},
			Spec: armadav1.ConfigBundleSpec{
				Datacenter: datacenter,
				Servers: []armadav1.ServerSpec{
					{
						ServiceTag: "3RK3V64",
						Idrac: armadav1.IdracSpec{
							SSHEnabled:    true,
							RacadmEnabled: false,
						},
					},
				},
			},
		}
		Expect(k8sClient.Patch(ctx, adminApply, client.Apply,
			client.FieldOwner("local:admin"),
			client.ForceOwnership,
		)).To(Succeed())

		// Verify admin owns both fields before takeover.
		var cbBefore armadav1.ConfigBundle
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: datacenter, Namespace: ns}, &cbBefore)).To(Succeed())
		adminTags := adminOwnedServiceTags(cbBefore.ManagedFields)
		Expect(adminTags).To(HaveKey("3RK3V64"), "admin should own the server entry")

		// Step 3: Run takeover — reclaim ONLY sshEnabled, leave racadmEnabled with admin.
		spec := armadav1.ConfigBundleSpec{
			Datacenter: datacenter,
			Servers: []armadav1.ServerSpec{
				{
					ServiceTag: "3RK3V64",
					Hostname:   "colo-r740-01",
					OobIP:      "10.10.1.45",
					Idrac: armadav1.IdracSpec{
						SSHEnabled:    false,
						RacadmEnabled: true,
					},
				},
			},
			Takeover: []armadav1.TakeoverEntry{
				{OrbID: "colo:srv-001-idrac", ServiceTag: "3RK3V64", Field: "sshEnabled"},
			},
		}
		Expect(server.processTakeover(ctx, spec)).To(Succeed())

		// Step 4: Read the CR back and verify field values.
		var cbAfter armadav1.ConfigBundle
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: datacenter, Namespace: ns}, &cbAfter)).To(Succeed())

		srv := findServerByTag(cbAfter.Spec.Servers, "3RK3V64")
		Expect(srv).NotTo(BeNil())

		// sshEnabled should be reclaimed to controller's value (false).
		Expect(srv.Idrac.SSHEnabled).To(BeFalse(), "sshEnabled should be reclaimed to controller intent (false)")

		// racadmEnabled should still be the admin's override value (false).
		Expect(srv.Idrac.RacadmEnabled).To(BeFalse(), "racadmEnabled should still be admin's override value")

		// Verify managedFields: sshEnabled should now be owned by configbundle-controller.
		// local:admin should still own racadmEnabled but NOT sshEnabled.
		adminPaths := extractAdminPaths(cbAfter.ManagedFields)
		var adminOwnsSSH, adminOwnsRacadm bool
		for _, ap := range adminPaths {
			if ap.path == "spec.servers[serviceTag=3RK3V64].idrac.sshEnabled" {
				adminOwnsSSH = true
			}
			if ap.path == "spec.servers[serviceTag=3RK3V64].idrac.racadmEnabled" {
				adminOwnsRacadm = true
			}
		}
		Expect(adminOwnsSSH).To(BeFalse(), "local:admin should no longer own sshEnabled after takeover")
		Expect(adminOwnsRacadm).To(BeTrue(), "local:admin should still own racadmEnabled (not targeted by takeover)")
	})

	It("succeeds with empty takeover list", func() {
		spec := armadav1.ConfigBundleSpec{Datacenter: "colo"}
		Expect(server.processTakeover(ctx, spec)).To(Succeed())
	})

	It("returns error when targeting a nonexistent server", func() {
		const datacenter = "colo"

		controllerApply := &armadav1.ConfigBundle{
			TypeMeta:   metav1.TypeMeta{APIVersion: armadav1.GroupVersion.String(), Kind: "ConfigBundle"},
			ObjectMeta: metav1.ObjectMeta{Name: datacenter, Namespace: ns},
			Spec: armadav1.ConfigBundleSpec{
				Datacenter: datacenter,
				Servers: []armadav1.ServerSpec{
					{ServiceTag: "3RK3V64", Hostname: "colo-r740-01", OobIP: "10.10.1.45"},
				},
			},
		}
		Expect(k8sClient.Patch(ctx, controllerApply, client.Apply,
			client.FieldOwner("configbundle-controller"),
		)).To(Succeed())

		spec := armadav1.ConfigBundleSpec{
			Datacenter: datacenter,
			Servers:    controllerApply.Spec.Servers,
			Takeover: []armadav1.TakeoverEntry{
				{OrbID: "x", ServiceTag: "NONEXISTENT", Field: "sshEnabled"},
			},
		}
		err := server.processTakeover(ctx, spec)
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("takeover entries failed"))
	})
})

// singleServerBundle returns a ConfigBundle with one server entry for use in tests.
func singleServerBundle(name, ns, hostname, serviceTag, oobIP string) *armadav1.ConfigBundle {
	return &armadav1.ConfigBundle{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: ns,
		},
		Spec: armadav1.ConfigBundleSpec{
			Datacenter: "colo",
			Servers: []armadav1.ServerSpec{
				{ServiceTag: serviceTag, Hostname: hostname, OobIP: oobIP},
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
