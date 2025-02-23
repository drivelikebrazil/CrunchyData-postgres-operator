//go:build envtest
// +build envtest

/*
 Copyright 2021 - 2023 Crunchy Data Solutions, Inc.
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

package postgrescluster

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"testing"

	"gotest.tools/v3/assert"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/crunchydata/postgres-operator/internal/initialize"
	"github.com/crunchydata/postgres-operator/internal/naming"
	"github.com/crunchydata/postgres-operator/internal/pgmonitor"
	"github.com/crunchydata/postgres-operator/internal/testing/require"
	"github.com/crunchydata/postgres-operator/pkg/apis/postgres-operator.crunchydata.com/v1beta1"
)

func TestAddPGMonitorExporterToInstancePodSpec(t *testing.T) {
	image := "test/image:tag"

	cluster := &v1beta1.PostgresCluster{}
	cluster.Spec.Port = initialize.Int32(5432)
	cluster.Spec.Image = image
	cluster.Spec.ImagePullPolicy = corev1.PullAlways

	resources := corev1.ResourceRequirements{
		Requests: corev1.ResourceList{
			corev1.ResourceCPU: resource.MustParse("100m"),
		},
	}

	getContainerWithName := func(containers []corev1.Container, name string) corev1.Container {
		for _, container := range containers {
			if container.Name == name {
				return container
			}
		}
		return corev1.Container{}
	}

	t.Run("ExporterDisabled", func(t *testing.T) {
		template := &corev1.PodTemplateSpec{}
		assert.NilError(t, addPGMonitorExporterToInstancePodSpec(cluster, template, nil))
		assert.DeepEqual(t, template, &corev1.PodTemplateSpec{})
	})

	t.Run("ExporterEnabled", func(t *testing.T) {
		cluster.Spec.Monitoring = &v1beta1.MonitoringSpec{
			PGMonitor: &v1beta1.PGMonitorSpec{
				Exporter: &v1beta1.ExporterSpec{
					Image:     image,
					Resources: resources,
				},
			},
		}
		template := &corev1.PodTemplateSpec{
			Spec: corev1.PodSpec{
				Containers: []corev1.Container{{
					Name: naming.ContainerDatabase,
				}},
			},
		}
		assert.NilError(t, addPGMonitorExporterToInstancePodSpec(cluster, template, nil))
		container := getContainerWithName(template.Spec.Containers, naming.ContainerPGMonitorExporter)
		assert.Equal(t, container.Image, image)
		assert.Equal(t, container.ImagePullPolicy, corev1.PullAlways)
		assert.DeepEqual(t, container.Resources, resources)
		assert.DeepEqual(t, container.Command, []string{"/opt/cpm/bin/start.sh"})
		assert.DeepEqual(t, container.SecurityContext.Capabilities, &corev1.Capabilities{
			Drop: []corev1.Capability{"ALL"},
		})
		assert.Equal(t, *container.SecurityContext.Privileged, false)
		assert.Equal(t, *container.SecurityContext.ReadOnlyRootFilesystem, true)
		assert.Equal(t, *container.SecurityContext.AllowPrivilegeEscalation, false)
		assert.Equal(t, *container.Resources.Requests.Cpu(), resource.MustParse("100m"))

		expectedENV := []corev1.EnvVar{
			{Name: "CONFIG_DIR", Value: "/opt/cpm/conf"},
			{Name: "POSTGRES_EXPORTER_PORT", Value: "9187"},
			{Name: "PGBACKREST_INFO_THROTTLE_MINUTES", Value: "10"},
			{Name: "PG_STAT_STATEMENTS_LIMIT", Value: "20"},
			{Name: "PG_STAT_STATEMENTS_THROTTLE_MINUTES", Value: "-1"},
			{Name: "EXPORTER_PG_HOST", Value: "localhost"},
			{Name: "EXPORTER_PG_PORT", Value: fmt.Sprint(*cluster.Spec.Port)},
			{Name: "EXPORTER_PG_DATABASE", Value: "postgres"},
			{Name: "EXPORTER_PG_USER", Value: pgmonitor.MonitoringUser},
			{Name: "EXPORTER_PG_PASSWORD", ValueFrom: &corev1.EnvVarSource{
				SecretKeyRef: &corev1.SecretKeySelector{
					LocalObjectReference: corev1.LocalObjectReference{
						Name: naming.MonitoringUserSecret(cluster).Name,
					},
					Key: "password",
				},
			}}}
		assert.DeepEqual(t, container.Env, expectedENV)

		assert.Assert(t, container.Ports[0].ContainerPort == int32(9187))
		assert.Assert(t, container.Ports[0].Name == "exporter")
		assert.Assert(t, container.Ports[0].Protocol == "TCP")

		assert.Assert(t, template.Spec.Volumes != nil)
	})

	t.Run("CustomConfig", func(t *testing.T) {
		cluster.Spec.Monitoring = &v1beta1.MonitoringSpec{
			PGMonitor: &v1beta1.PGMonitorSpec{
				Exporter: &v1beta1.ExporterSpec{
					Image:     image,
					Resources: resources,
					Configuration: []corev1.VolumeProjection{{ConfigMap: &corev1.ConfigMapProjection{
						LocalObjectReference: corev1.LocalObjectReference{
							Name: "exporter-custom-config-test",
						},
					}},
					},
				},
			},
		}
		template := &corev1.PodTemplateSpec{
			Spec: corev1.PodSpec{
				Containers: []corev1.Container{{
					Name: naming.ContainerDatabase,
				}},
			},
		}

		assert.NilError(t, addPGMonitorExporterToInstancePodSpec(cluster, template, nil))

		var foundConfigVolume bool
		for _, v := range template.Spec.Volumes {
			if v.Name == "exporter-config" {
				assert.DeepEqual(t, v, corev1.Volume{
					Name: "exporter-config",
					VolumeSource: corev1.VolumeSource{
						Projected: &corev1.ProjectedVolumeSource{
							Sources: cluster.Spec.Monitoring.PGMonitor.Exporter.Configuration,
						},
					},
				})
				foundConfigVolume = true
				break
			}
		}
		assert.Assert(t, foundConfigVolume)

		container := getContainerWithName(template.Spec.Containers, naming.ContainerPGMonitorExporter)
		var foundConfigMount bool
		for _, vm := range container.VolumeMounts {
			if vm.Name == "exporter-config" && vm.MountPath == "/conf" {
				foundConfigMount = true
				break
			}
		}
		assert.Assert(t, foundConfigMount)
	})
}

// TestReconcilePGMonitorExporterSetupErrors tests how reconcilePGMonitorExporter
// reacts when the kubernetes resources are in different states (e.g., checks
// what happens when the database pod is terminating)
func TestReconcilePGMonitorExporterSetupErrors(t *testing.T) {
	for _, test := range []struct {
		name          string
		podExecCalled bool
		status        v1beta1.MonitoringStatus
		monitoring    *v1beta1.MonitoringSpec
		instances     []*Instance
		secret        *corev1.Secret
	}{{
		name:          "Terminating",
		podExecCalled: false,
		instances: []*Instance{
			{
				Name: "daisy",
				Pods: []*corev1.Pod{{
					ObjectMeta: metav1.ObjectMeta{
						Name:              "daisy-pod",
						Annotations:       map[string]string{"status": `{"role":"master"}`},
						DeletionTimestamp: &metav1.Time{},
					},
				}},
				Runner: &appsv1.StatefulSet{},
			},
		},
	}, {
		name:          "NotWritable",
		podExecCalled: false,
		instances: []*Instance{
			{
				Name: "daisy",
				Pods: []*corev1.Pod{{
					ObjectMeta: metav1.ObjectMeta{
						Name: "daisy-pod",
					},
				}},
				Runner: &appsv1.StatefulSet{},
			},
		},
	}, {
		name:          "NotRunning",
		podExecCalled: false,
		instances: []*Instance{
			{
				Name: "daisy",
				Pods: []*corev1.Pod{{
					ObjectMeta: metav1.ObjectMeta{
						Name:        "daisy-pod",
						Annotations: map[string]string{"status": `{"role":"master"}`},
					},
				}},
				Runner: &appsv1.StatefulSet{},
			},
		},
	}, {
		name:          "ExporterNotRunning",
		podExecCalled: false,
		monitoring: &v1beta1.MonitoringSpec{
			PGMonitor: &v1beta1.PGMonitorSpec{
				Exporter: &v1beta1.ExporterSpec{
					Image: "image",
				},
			},
		},
		instances: []*Instance{
			{
				Name: "daisy",
				Pods: []*corev1.Pod{{
					ObjectMeta: metav1.ObjectMeta{
						Name:        "daisy-pod",
						Annotations: map[string]string{"status": `{"role":"master"}`},
					},
					Status: corev1.PodStatus{
						ContainerStatuses: []corev1.ContainerStatus{{
							Name:  naming.ContainerDatabase,
							State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}},
						}},
					},
				}},
				Runner: &appsv1.StatefulSet{},
			},
		},
	}, {
		name:          "ExporterImageIDNotFound",
		podExecCalled: false,
		monitoring: &v1beta1.MonitoringSpec{
			PGMonitor: &v1beta1.PGMonitorSpec{
				Exporter: &v1beta1.ExporterSpec{
					Image: "image",
				},
			},
		},
		instances: []*Instance{
			{
				Name: "daisy",
				Pods: []*corev1.Pod{{
					ObjectMeta: metav1.ObjectMeta{
						Name:        "daisy-pod",
						Annotations: map[string]string{"status": `{"role":"master"}`},
					},
					Status: corev1.PodStatus{
						ContainerStatuses: []corev1.ContainerStatus{{
							Name:  naming.ContainerDatabase,
							State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}},
						}, {
							Name:  naming.ContainerPGMonitorExporter,
							State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}},
						}},
					},
				}},
				Runner: &appsv1.StatefulSet{},
			},
		},
	}, {
		name:          "NoError",
		podExecCalled: true,
		monitoring: &v1beta1.MonitoringSpec{
			PGMonitor: &v1beta1.PGMonitorSpec{
				Exporter: &v1beta1.ExporterSpec{
					Image: "image",
				},
			},
		},
		instances: []*Instance{
			{
				Name: "daisy",
				Pods: []*corev1.Pod{{
					ObjectMeta: metav1.ObjectMeta{
						Name:        "daisy-pod",
						Annotations: map[string]string{"status": `{"role":"master"}`},
					},
					Status: corev1.PodStatus{
						ContainerStatuses: []corev1.ContainerStatus{{
							Name:    naming.ContainerDatabase,
							State:   corev1.ContainerState{Running: &corev1.ContainerStateRunning{}},
							ImageID: "image@sha123",
						}, {
							Name:    naming.ContainerPGMonitorExporter,
							State:   corev1.ContainerState{Running: &corev1.ContainerStateRunning{}},
							ImageID: "image@sha123",
						}},
					},
				}},
				Runner: &appsv1.StatefulSet{},
			},
		},
		secret: &corev1.Secret{
			Data: map[string][]byte{
				"verifier": []byte("blah"),
			},
		},
	}} {
		t.Run(test.name, func(t *testing.T) {
			ctx := context.Background()
			var called bool
			reconciler := &Reconciler{
				PodExec: func(namespace, pod, container string, stdin io.Reader, stdout,
					stderr io.Writer, command ...string) error {
					called = true
					return nil
				},
			}

			cluster := &v1beta1.PostgresCluster{}
			cluster.Spec.Monitoring = test.monitoring
			cluster.Status.Monitoring.ExporterConfiguration = test.status.ExporterConfiguration
			observed := &observedInstances{forCluster: test.instances}

			assert.NilError(t, reconciler.reconcilePGMonitorExporter(ctx,
				cluster, observed, test.secret))
			assert.Equal(t, called, test.podExecCalled)
		})
	}
}

func TestReconcilePGMonitorExporter(t *testing.T) {
	ctx := context.Background()
	var called bool
	reconciler := &Reconciler{
		PodExec: func(namespace, pod, container string, stdin io.Reader, stdout,
			stderr io.Writer, command ...string) error {
			called = true
			return nil
		},
	}

	t.Run("UninstallWhenSecretNil", func(t *testing.T) {
		cluster := &v1beta1.PostgresCluster{}
		cluster.Status.Monitoring.ExporterConfiguration = "installed"
		instances := []*Instance{
			{
				Name: "one-daisy",
				Pods: []*corev1.Pod{{
					ObjectMeta: metav1.ObjectMeta{
						Name:        "one-daisy-pod",
						Annotations: map[string]string{"status": `{"role":"master"}`},
					},
					Status: corev1.PodStatus{
						Phase: corev1.PodRunning,
						ContainerStatuses: []corev1.ContainerStatus{{
							Name:    naming.ContainerDatabase,
							ImageID: "dont-care",
							State: corev1.ContainerState{
								Running: &corev1.ContainerStateRunning{},
							},
						}},
					},
				}},
				Runner: &appsv1.StatefulSet{},
			},
		}
		observed := &observedInstances{forCluster: instances}

		called = false
		assert.NilError(t, reconciler.reconcilePGMonitorExporter(ctx,
			cluster, observed, nil))
		assert.Assert(t, called)
		assert.Assert(t, cluster.Status.Monitoring.ExporterConfiguration != "")
	})
}

// TestReconcilePGMonitorExporterStatus checks that the exporter status is updated
// when it should be. Because the status updated when we update the setup sql from
// pgmonitor (by using podExec), we check if podExec is called when a change is needed.
func TestReconcilePGMonitorExporterStatus(t *testing.T) {
	for _, test := range []struct {
		name                        string
		exporterEnabled             bool
		podExecCalled               bool
		status                      v1beta1.MonitoringStatus
		statusChangedAfterReconcile bool
	}{{
		name:                        "Disabled",
		podExecCalled:               true,
		statusChangedAfterReconcile: true,
	}, {
		name:                        "Disabled Uninstall",
		podExecCalled:               true,
		status:                      v1beta1.MonitoringStatus{ExporterConfiguration: "installed"},
		statusChangedAfterReconcile: true,
	}, {
		name:                        "Enabled",
		exporterEnabled:             true,
		podExecCalled:               true,
		statusChangedAfterReconcile: true,
	}, {
		name:                        "Enabled Update",
		exporterEnabled:             true,
		podExecCalled:               true,
		status:                      v1beta1.MonitoringStatus{ExporterConfiguration: "installed"},
		statusChangedAfterReconcile: true,
	}, {
		name:            "Enabled NoUpdate",
		exporterEnabled: true,
		podExecCalled:   false,
		// Status was generated manually for this test case
		// TODO jmckulk: add code to generate status
		status:                      v1beta1.MonitoringStatus{ExporterConfiguration: "74476b9895"},
		statusChangedAfterReconcile: false,
	}} {
		t.Run(test.name, func(t *testing.T) {
			ctx := context.Background()
			var (
				called bool
				secret *corev1.Secret
			)

			// Create reconciler with mock PodExec function
			reconciler := &Reconciler{
				PodExec: func(namespace, pod, container string, stdin io.Reader, stdout,
					stderr io.Writer, command ...string) error {
					called = true
					return nil
				},
			}

			// Create the test cluster spec with the exporter status set
			cluster := &v1beta1.PostgresCluster{}
			cluster.Status.Monitoring.ExporterConfiguration = test.status.ExporterConfiguration

			// Mock up an instances that will be defined in the cluster. The instances should
			// have all necessary fields that will be needed to reconcile the exporter
			instances := []*Instance{
				{
					Name: "daisy",
					Pods: []*corev1.Pod{{
						ObjectMeta: metav1.ObjectMeta{
							Name:        "daisy-pod",
							Annotations: map[string]string{"status": `{"role":"master"}`},
						},
						Status: corev1.PodStatus{
							ContainerStatuses: []corev1.ContainerStatus{{
								Name:    naming.ContainerDatabase,
								State:   corev1.ContainerState{Running: &corev1.ContainerStateRunning{}},
								ImageID: "image@sha123",
							}},
						},
					}},
					Runner: &appsv1.StatefulSet{},
				},
			}

			if test.exporterEnabled {
				// When testing with exporter enabled update the spec with exporter fields
				cluster.Spec.Monitoring = &v1beta1.MonitoringSpec{
					PGMonitor: &v1beta1.PGMonitorSpec{
						Exporter: &v1beta1.ExporterSpec{
							Image: "image",
						},
					},
				}

				// Update mock instances to include the exporter container
				instances[0].Pods[0].Status.ContainerStatuses = append(
					instances[0].Pods[0].Status.ContainerStatuses, corev1.ContainerStatus{
						Name:    naming.ContainerPGMonitorExporter,
						State:   corev1.ContainerState{Running: &corev1.ContainerStateRunning{}},
						ImageID: "image@sha123",
					})

				secret = &corev1.Secret{
					Data: map[string][]byte{
						"verifier": []byte("blah"),
					},
				}
			}

			// Mock up observed instances based on our mock instances
			observed := &observedInstances{forCluster: instances}

			// Check that we can reconcile with the test resources
			assert.NilError(t, reconciler.reconcilePGMonitorExporter(ctx,
				cluster, observed, secret))
			// Check that the exporter status changes when it needs to
			assert.Assert(t, test.statusChangedAfterReconcile == (cluster.Status.Monitoring.ExporterConfiguration != test.status.ExporterConfiguration),
				"got %v", cluster.Status.Monitoring.ExporterConfiguration)
			// Check that pod exec is called correctly
			assert.Equal(t, called, test.podExecCalled)
		})
	}
}

// TestReconcileMonitoringSecret checks that the secret intent returned by reconcileMonitoringSecret
// is correct. If exporter is enabled, the return shouldn't be nil. If the exporter is disabled, the
// return should be nil.
func TestReconcileMonitoringSecret(t *testing.T) {
	// TODO jmckulk: debug test with existing cluster
	// Seems to be an issue when running with other tests
	if strings.EqualFold(os.Getenv("USE_EXISTING_CLUSTER"), "true") {
		t.Skip("Test failing with existing cluster")
	}

	ctx := context.Background()

	// Kubernetes is required because reconcileMonitoringSecret
	// (1) uses the client to get existing secrets
	// (2) sets the controller reference on the new secret
	_, cc := setupKubernetes(t)
	require.ParallelCapacity(t, 0)

	reconciler := &Reconciler{Client: cc, Owner: client.FieldOwner(t.Name())}

	cluster := testCluster()
	cluster.Default()
	cluster.UID = types.UID("hippouid")
	cluster.Namespace = setupNamespace(t, cc).Name

	t.Run("ExporterDisabled", func(t *testing.T) {
		t.Run("NotExisting", func(t *testing.T) {
			secret, err := reconciler.reconcileMonitoringSecret(ctx, cluster)
			assert.NilError(t, err)
			assert.Assert(t, secret == nil)
		})

		t.Run("Existing", func(t *testing.T) {
			cluster.Spec.Monitoring = &v1beta1.MonitoringSpec{
				PGMonitor: &v1beta1.PGMonitorSpec{
					Exporter: &v1beta1.ExporterSpec{Image: "image"}}}
			existing, err := reconciler.reconcileMonitoringSecret(ctx, cluster)
			assert.NilError(t, err, "error in test; existing secret not created")
			assert.Assert(t, existing != nil, "error in test; existing secret not created")

			cluster.Spec.Monitoring = nil
			actual, err := reconciler.reconcileMonitoringSecret(ctx, cluster)
			assert.NilError(t, err)
			assert.Assert(t, actual == nil)
		})
	})

	t.Run("ExporterEnabled", func(t *testing.T) {
		var (
			existing, actual *corev1.Secret
			err              error
		)

		// Enable monitoring in the test cluster spec
		cluster.Spec.Monitoring = &v1beta1.MonitoringSpec{
			PGMonitor: &v1beta1.PGMonitorSpec{
				Exporter: &v1beta1.ExporterSpec{
					Image: "image",
				},
			},
		}

		t.Run("NotExisting", func(t *testing.T) {
			existing, err = reconciler.reconcileMonitoringSecret(ctx, cluster)
			assert.NilError(t, err)
			assert.Assert(t, existing != nil)
		})

		t.Run("Existing", func(t *testing.T) {
			actual, err = reconciler.reconcileMonitoringSecret(ctx, cluster)
			assert.NilError(t, err)
			assert.Assert(t, bytes.Equal(actual.Data["password"], existing.Data["password"]))
		})
	})
}

// TestConfigureExporterTLS checks that tls settings are configured on a podTemplate.
// When exporter is enabled with custom tls configureExporterTLS should add volumes,
// volumeMounts, and an envVar to the template. Ensure that existing template configurations
// are still present
func TestConfigreExporterTLS(t *testing.T) {
	// Define an existing template with values that could be overwritten
	baseTemplate := &corev1.PodTemplateSpec{
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{{
				Name: naming.ContainerPGMonitorExporter,
				VolumeMounts: []corev1.VolumeMount{{
					Name:      "existing-volume",
					MountPath: "some-path",
				}},
				Env: []corev1.EnvVar{{
					Name:  "existing-env",
					Value: "existing-value",
				}},
			}},
			Volumes: []corev1.Volume{{
				Name: "existing-volume",
				VolumeSource: corev1.VolumeSource{
					EmptyDir: &corev1.EmptyDirVolumeSource{},
				},
			}},
		},
	}

	t.Run("Exporter disabled", func(t *testing.T) {
		cluster := &v1beta1.PostgresCluster{}
		template := baseTemplate.DeepCopy()
		configureExporterTLS(cluster, template, nil)
		// Template shouldn't have changed
		assert.DeepEqual(t, template, baseTemplate)
	})

	t.Run("Exporter enabled no tls", func(t *testing.T) {
		cluster := &v1beta1.PostgresCluster{
			Spec: v1beta1.PostgresClusterSpec{
				Monitoring: &v1beta1.MonitoringSpec{
					PGMonitor: &v1beta1.PGMonitorSpec{
						Exporter: &v1beta1.ExporterSpec{},
					},
				},
			},
		}
		template := baseTemplate.DeepCopy()
		configureExporterTLS(cluster, template, nil)
		// Template shouldn't have changed
		assert.DeepEqual(t, template, baseTemplate)
	})

	t.Run("Custom TLS provided", func(t *testing.T) {
		cluster := &v1beta1.PostgresCluster{
			ObjectMeta: metav1.ObjectMeta{Name: "test"},
			Spec: v1beta1.PostgresClusterSpec{
				Monitoring: &v1beta1.MonitoringSpec{
					PGMonitor: &v1beta1.PGMonitorSpec{
						Exporter: &v1beta1.ExporterSpec{
							CustomTLSSecret: &corev1.SecretProjection{
								LocalObjectReference: corev1.LocalObjectReference{
									Name: "custom-exporter-certs",
								},
							},
						},
					},
				},
			},
		}
		template := baseTemplate.DeepCopy()

		testConfigMap := &corev1.ConfigMap{
			ObjectMeta: naming.ExporterWebConfigMap(cluster),
		}

		// What happens if the template already includes volumes/Mounts and envs?
		configureExporterTLS(cluster, template, testConfigMap)

		// Did we configure the cert volume and the web config volume while leaving
		// existing volumes in place?
		assert.Assert(t, marshalMatches(template.Spec.Volumes, `
- emptyDir: {}
  name: existing-volume
- name: exporter-certs
  projected:
    sources:
    - secret:
        name: custom-exporter-certs
- configMap:
    name: test-exporter-web-config
  name: web-config
		`))

		// Is the exporter container in position 0?
		assert.Assert(t, template.Spec.Containers[0].Name == naming.ContainerPGMonitorExporter)

		// Did we configure the volume mounts on the container while leaving existing
		// mounts in place?
		assert.Assert(t, marshalMatches(template.Spec.Containers[0].VolumeMounts, `
- mountPath: some-path
  name: existing-volume
- mountPath: /certs
  name: exporter-certs
- mountPath: /web-config
  name: web-config
		`))

		// Did we set the `WEB_CONFIG_DIR` env var on the container while leaving
		// existing vars in place?
		assert.Assert(t, marshalMatches(template.Spec.Containers[0].Env, `
- name: existing-env
  value: existing-value
- name: WEB_CONFIG_DIR
  value: web-config/
		`))
	})
}
