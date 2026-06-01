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
	"fmt"
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

// ---------------------------------------------------------------------------
// Fakes for Puller envtest tests
// ---------------------------------------------------------------------------

type fakeOCIClient struct {
	artifact *OCIArtifact
	err      error
}

func (f *fakeOCIClient) Pull(_ context.Context, _ string) (*OCIArtifact, error) {
	return f.artifact, f.err
}

type fakeOrbClient struct {
	called bool
	err    error
}

func (f *fakeOrbClient) ImportSubgraph(_ context.Context, _, _ []byte) error {
	f.called = true
	return f.err
}

// testManifest builds a minimal ConfigBundle manifest YAML for use in Puller tests.
func testManifest(datacenter string, servers ...armadav1.ServerSpec) []byte {
	yaml := fmt.Sprintf("datacenter: %s\n", datacenter)
	if len(servers) > 0 {
		yaml += "servers:\n"
		for _, s := range servers {
			yaml += fmt.Sprintf("  - serviceTag: %q\n    hostname: %q\n    oobIP: %q\n",
				s.ServiceTag, s.Hostname, s.OobIP)
		}
	}
	return []byte(yaml)
}

// ---------------------------------------------------------------------------
// Puller envtest tests
// ---------------------------------------------------------------------------

// Describe block is at package scope so it registers with the suite in suite_test.go.
var _ = Describe("Puller", func() {
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
		ns = fmt.Sprintf("puller-%d", nsCounter)
		Expect(k8sClient.Create(ctx, &corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{Name: ns},
		})).To(Succeed())
	})

	AfterEach(func() {
		Expect(k8sClient.Delete(ctx, &corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{Name: ns},
		})).To(Succeed())
	})

	newPuller := func(oci OCIClient, orb OrbClient, datacenter string) *Puller {
		return &Puller{
			Client: k8sClient,
			Config: PullerConfig{
				RegistryURL:      "localhost:5000",
				PollInterval:     time.Hour, // not used in tests — RunCycle called directly
				Datacenter:       datacenter,
				Namespace:        ns,
				OrbImportEnabled: true,
			},
			OCI: oci,
			Orb: orb,
		}
	}

	It("skips the cycle when the digest is unchanged", func() {
		const digest = "sha256:aabbcc"
		const datacenter = "colo"

		// Pre-create the ConfigBundle CR with lastAppliedDigest already set.
		cb := &armadav1.ConfigBundle{
			ObjectMeta: metav1.ObjectMeta{Name: datacenter, Namespace: ns},
			Spec:       armadav1.ConfigBundleSpec{Datacenter: datacenter},
		}
		Expect(k8sClient.Create(ctx, cb)).To(Succeed())
		cb.Status.LastAppliedDigest = digest
		Expect(k8sClient.Status().Update(ctx, cb)).To(Succeed())

		orb := &fakeOrbClient{}
		puller := newPuller(&fakeOCIClient{
			artifact: &OCIArtifact{Digest: digest, Manifest: testManifest(datacenter)},
		}, orb, datacenter)

		Expect(puller.RunCycle(ctx)).To(Succeed())

		// Orb must not be called when digest is unchanged.
		Expect(orb.called).To(BeFalse(), "orb must not be called when digest is unchanged")
	})

	It("aborts and does not write the CR when orb import fails", func() {
		const datacenter = "colo"
		orb := &fakeOrbClient{err: fmt.Errorf("orb unavailable")}
		puller := newPuller(&fakeOCIClient{
			artifact: &OCIArtifact{
				Digest:   "sha256:newdigest",
				Manifest: testManifest(datacenter, armadav1.ServerSpec{ServiceTag: "3RK3V64", Hostname: "colo-r740-01", OobIP: "10.0.0.1"}),
				Data:     []byte("data"),
				Schema:   []byte("schema"),
			},
		}, orb, datacenter)

		err := puller.RunCycle(ctx)
		Expect(err).To(HaveOccurred(), "RunCycle must return error when orb fails")
		Expect(err.Error()).To(ContainSubstring("orb import"))

		// ConfigBundle CR must not exist — cycle aborted before CR write.
		var cb armadav1.ConfigBundle
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: datacenter, Namespace: ns}, &cb)).
			To(MatchError(ContainSubstring("not found")))
	})

	It("creates the ConfigBundle CR and updates status on a successful cycle", func() {
		const datacenter = "colo"
		orb := &fakeOrbClient{}
		puller := newPuller(&fakeOCIClient{
			artifact: &OCIArtifact{
				Digest: "sha256:abc123",
				Manifest: testManifest(datacenter,
					armadav1.ServerSpec{ServiceTag: "3RK3V64", Hostname: "colo-r740-01", OobIP: "10.10.1.45"},
				),
				Data:   []byte("data"),
				Schema: []byte("schema"),
			},
		}, orb, datacenter)

		Expect(puller.RunCycle(ctx)).To(Succeed())
		Expect(orb.called).To(BeTrue())

		var cb armadav1.ConfigBundle
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: datacenter, Namespace: ns}, &cb)).To(Succeed())

		Expect(cb.Spec.Datacenter).To(Equal(datacenter))
		Expect(cb.Spec.Servers).To(HaveLen(1))
		Expect(cb.Spec.Servers[0].ServiceTag).To(Equal("3RK3V64"))

		Expect(cb.Status.LastAppliedDigest).To(Equal("sha256:abc123"))
		Expect(cb.Status.LastAppliedAt).NotTo(BeNil())
		Expect(conditionStatus(cb.Status.Conditions, armadav1.ConditionArtifactFetched)).
			To(Equal(metav1.ConditionTrue))
		Expect(conditionStatus(cb.Status.Conditions, armadav1.ConditionSignatureVerified)).
			To(Equal(metav1.ConditionTrue))
		Expect(conditionStatus(cb.Status.Conditions, armadav1.ConditionGraphImported)).
			To(Equal(metav1.ConditionTrue))
	})

	It("updates an existing ConfigBundle CR on a new digest", func() {
		const datacenter = "colo"
		orb := &fakeOrbClient{}
		oci := &fakeOCIClient{
			artifact: &OCIArtifact{
				Digest:   "sha256:first",
				Manifest: testManifest(datacenter, armadav1.ServerSpec{ServiceTag: "3RK3V64", Hostname: "colo-r740-01", OobIP: "10.10.1.45"}),
				Data:     []byte("d"),
				Schema:   []byte("s"),
			},
		}
		puller := newPuller(oci, orb, datacenter)

		// First cycle creates the CR.
		Expect(puller.RunCycle(ctx)).To(Succeed())

		// Second cycle with a new digest — must update spec and status.
		oci.artifact = &OCIArtifact{
			Digest: "sha256:second",
			Manifest: testManifest(datacenter,
				armadav1.ServerSpec{ServiceTag: "3RK3V64", Hostname: "colo-r740-01", OobIP: "10.10.1.45"},
				armadav1.ServerSpec{ServiceTag: "FQK3V64", Hostname: "colo-r740-02", OobIP: "10.10.1.46"},
			),
			Data:   []byte("d2"),
			Schema: []byte("s2"),
		}
		Expect(puller.RunCycle(ctx)).To(Succeed())

		var cb armadav1.ConfigBundle
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: datacenter, Namespace: ns}, &cb)).To(Succeed())
		Expect(cb.Status.LastAppliedDigest).To(Equal("sha256:second"))
		Expect(cb.Spec.Servers).To(HaveLen(2))
	})

	It("omits local:admin-owned server entries from the SSA patch", func() {
		const datacenter = "colo"

		// Step 1: local:admin applies and claims server A.
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

		// Step 2: Puller cycle with two servers — A (admin-owned) and B (uncontested).
		orb := &fakeOrbClient{}
		puller := newPuller(&fakeOCIClient{
			artifact: &OCIArtifact{
				Digest: "sha256:newdigest",
				Manifest: testManifest(datacenter,
					armadav1.ServerSpec{ServiceTag: "AAA0001", Hostname: "colo-r740-01", OobIP: "10.10.1.45"},
					armadav1.ServerSpec{ServiceTag: "BBB0002", Hostname: "colo-r740-02", OobIP: "10.10.1.46"},
				),
				Data:   []byte("data"),
				Schema: []byte("schema"),
			},
		}, orb, datacenter)

		Expect(puller.RunCycle(ctx)).To(Succeed())

		var cb armadav1.ConfigBundle
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: datacenter, Namespace: ns}, &cb)).To(Succeed())

		// Server B must be present (Puller owns it).
		serverB := findServerByTag(cb.Spec.Servers, "BBB0002")
		Expect(serverB).NotTo(BeNil(), "server B must be present after Puller cycle")

		// Server A must still be present (admin's entry preserved by SSA).
		serverA := findServerByTag(cb.Spec.Servers, "AAA0001")
		Expect(serverA).NotTo(BeNil(), "server A must still be present")
		// Admin's override must be intact.
		Expect(serverA.Idrac.SSHEnabled).To(BeTrue(), "admin override on server A must be preserved")

		// Orb must have been called.
		Expect(orb.called).To(BeTrue())
	})

	It("does not call orb when OCI pull fails", func() {
		const datacenter = "colo"
		orb := &fakeOrbClient{}
		puller := newPuller(&fakeOCIClient{err: fmt.Errorf("zot unreachable")}, orb, datacenter)

		err := puller.RunCycle(ctx)
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("OCI pull"))
		Expect(orb.called).To(BeFalse())
	})

	It("processes a subsequent cycle when a second new digest arrives", func() {
		const datacenter = "colo"
		orb := &fakeOrbClient{}

		// First cycle — digest A.
		oci := &fakeOCIClient{artifact: &OCIArtifact{
			Digest:   "sha256:digestA",
			Manifest: testManifest(datacenter),
			Data:     []byte("d"), Schema: []byte("s"),
		}}
		Expect(newPuller(oci, orb, datacenter).RunCycle(ctx)).To(Succeed())

		// Same digest — no orb call.
		orb.called = false
		Expect(newPuller(oci, orb, datacenter).RunCycle(ctx)).To(Succeed())
		Expect(orb.called).To(BeFalse(), "same digest must not trigger orb call")

		// New digest — orb called again.
		oci.artifact = &OCIArtifact{
			Digest:   "sha256:digestB",
			Manifest: testManifest(datacenter),
			Data:     []byte("d2"), Schema: []byte("s2"),
		}
		orb.called = false
		Expect(newPuller(oci, orb, datacenter).RunCycle(ctx)).To(Succeed())
		Expect(orb.called).To(BeTrue(), "new digest must trigger orb call")
	})

	It("sets GraphImported condition to False/Disabled when OrbImportEnabled=false", func() {
		const datacenter = "colo"
		orb := &fakeOrbClient{}
		puller := &Puller{
			Client: k8sClient,
			Config: PullerConfig{
				RegistryURL:      "localhost:5000",
				PollInterval:     time.Hour,
				Datacenter:       datacenter,
				Namespace:        ns,
				OrbImportEnabled: false,
			},
			OCI: &fakeOCIClient{
				artifact: &OCIArtifact{
					Digest:   "sha256:abc123",
					Manifest: testManifest(datacenter, armadav1.ServerSpec{ServiceTag: "3RK3V64", Hostname: "colo-r740-01", OobIP: "10.10.1.45"}),
					Data:     []byte("data"),
					Schema:   []byte("schema"),
				},
			},
			Orb: orb,
		}

		Expect(puller.RunCycle(ctx)).To(Succeed())
		Expect(orb.called).To(BeFalse(), "orb must not be called when OrbImportEnabled=false")

		var cb armadav1.ConfigBundle
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: datacenter, Namespace: ns}, &cb)).To(Succeed())
		Expect(conditionStatus(cb.Status.Conditions, armadav1.ConditionGraphImported)).
			To(Equal(metav1.ConditionFalse), "GraphImported must be False when orb import is disabled")
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
