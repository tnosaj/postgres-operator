//go:build envtest
// +build envtest

/*
 Copyright 2021 Crunchy Data Solutions, Inc.
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
	"context"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/pkg/errors"
	"go.opentelemetry.io/otel"
	"gotest.tools/v3/assert"
	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/crunchydata/postgres-operator/internal/initialize"
	"github.com/crunchydata/postgres-operator/internal/naming"
	"github.com/crunchydata/postgres-operator/internal/pgbackrest"
	"github.com/crunchydata/postgres-operator/pkg/apis/postgres-operator.crunchydata.com/v1beta1"
)

func TestInstanceIsRunning(t *testing.T) {
	var instance Instance
	var known, running bool

	// No pods
	running, known = instance.IsRunning("any")
	assert.Assert(t, !known)
	assert.Assert(t, !running)

	// No statuses
	instance.Pods = []*corev1.Pod{{}}
	running, known = instance.IsRunning("any")
	assert.Assert(t, !known)
	assert.Assert(t, !running)

	// No states
	instance.Pods[0].Status.ContainerStatuses = []corev1.ContainerStatus{{
		Name: "c1",
	}}
	running, known = instance.IsRunning("c1")
	assert.Assert(t, known)
	assert.Assert(t, !running)

	running, known = instance.IsRunning("missing")
	assert.Assert(t, !known)
	assert.Assert(t, !running)

	// Running state
	// - https://releases.k8s.io/v1.21.0/staging/src/k8s.io/kubectl/pkg/cmd/debug/debug.go#L668
	instance.Pods[0].Status.ContainerStatuses[0].State.Running =
		new(corev1.ContainerStateRunning)

	running, known = instance.IsRunning("c1")
	assert.Assert(t, known)
	assert.Assert(t, running)

	running, known = instance.IsRunning("missing")
	assert.Assert(t, !known)
	assert.Assert(t, !running)

	// Init containers
	instance.Pods[0].Status.InitContainerStatuses = []corev1.ContainerStatus{{
		Name: "i1",
		State: corev1.ContainerState{
			Running: new(corev1.ContainerStateRunning),
		},
	}}

	running, known = instance.IsRunning("i1")
	assert.Assert(t, known)
	assert.Assert(t, running)
}

func TestInstanceIsWritable(t *testing.T) {
	var instance Instance
	var known, writable bool

	// No pods
	writable, known = instance.IsWritable()
	assert.Assert(t, !known)
	assert.Assert(t, !writable)

	// No annotations
	instance.Pods = []*corev1.Pod{{}}
	writable, known = instance.IsWritable()
	assert.Assert(t, !known)
	assert.Assert(t, !writable)

	// No role
	instance.Pods[0].Annotations = map[string]string{"status": `{}`}
	writable, known = instance.IsWritable()
	assert.Assert(t, !known)
	assert.Assert(t, !writable)

	// Patroni leader
	instance.Pods[0].Annotations["status"] = `{"role":"master"}`
	writable, known = instance.IsWritable()
	assert.Assert(t, known)
	assert.Assert(t, writable)

	// Patroni replica
	instance.Pods[0].Annotations["status"] = `{"role":"replica"}`
	writable, known = instance.IsWritable()
	assert.Assert(t, known)
	assert.Assert(t, !writable)

	// Patroni standby leader
	instance.Pods[0].Annotations["status"] = `{"role":"standby_leader"}`
	writable, known = instance.IsWritable()
	assert.Assert(t, known)
	assert.Assert(t, !writable)
}

func TestNewObservedInstances(t *testing.T) {
	t.Run("Empty", func(t *testing.T) {
		cluster := new(v1beta1.PostgresCluster)
		observed := newObservedInstances(cluster, nil, nil)

		assert.Equal(t, len(observed.forCluster), 0)
		assert.Equal(t, len(observed.byName), 0)
		assert.Equal(t, len(observed.bySet), 0)
	})

	t.Run("PodMissingOthers", func(t *testing.T) {
		cluster := new(v1beta1.PostgresCluster)
		observed := newObservedInstances(
			cluster,
			nil,
			[]corev1.Pod{
				{
					ObjectMeta: metav1.ObjectMeta{
						Name: "some-pod-name",
						Labels: map[string]string{
							"postgres-operator.crunchydata.com/instance-set": "missing",
							"postgres-operator.crunchydata.com/instance":     "the-name",
						},
					},
				},
			})

		// Registers as an instance.
		assert.Equal(t, len(observed.forCluster), 1)
		assert.Equal(t, len(observed.byName), 1)
		assert.Equal(t, len(observed.bySet), 1)

		instance := observed.forCluster[0]
		assert.Equal(t, instance.Name, "the-name")
		assert.Equal(t, len(instance.Pods), 1)   // The Pod
		assert.Assert(t, instance.Runner == nil) // No matching StatefulSet
		assert.Assert(t, instance.Spec == nil)   // No matching PostgresInstanceSetSpec

		// Lookup based on its labels.
		assert.Equal(t, observed.byName["the-name"], instance)
		assert.DeepEqual(t, observed.bySet["missing"], []*Instance{instance})
		assert.DeepEqual(t, observed.setNames.List(), []string{"missing"})
	})

	t.Run("RunnerMissingOthers", func(t *testing.T) {
		cluster := new(v1beta1.PostgresCluster)
		observed := newObservedInstances(
			cluster,
			[]appsv1.StatefulSet{
				{
					ObjectMeta: metav1.ObjectMeta{
						Name: "the-name",
						Labels: map[string]string{
							"postgres-operator.crunchydata.com/instance-set": "missing",
						},
					},
				},
			},
			nil)

		// Registers as an instance.
		assert.Equal(t, len(observed.forCluster), 1)
		assert.Equal(t, len(observed.byName), 1)
		assert.Equal(t, len(observed.bySet), 1)

		instance := observed.forCluster[0]
		assert.Equal(t, instance.Name, "the-name")
		assert.Equal(t, len(instance.Pods), 0)   // No matching Pods
		assert.Assert(t, instance.Runner != nil) // The StatefulSet
		assert.Assert(t, instance.Spec == nil)   // No matching PostgresInstanceSetSpec

		// Lookup based on its name and labels.
		assert.Equal(t, observed.byName["the-name"], instance)
		assert.DeepEqual(t, observed.bySet["missing"], []*Instance{instance})
		assert.DeepEqual(t, observed.setNames.List(), []string{"missing"})
	})

	t.Run("Matching", func(t *testing.T) {
		cluster := new(v1beta1.PostgresCluster)
		cluster.Spec.InstanceSets = []v1beta1.PostgresInstanceSetSpec{{Name: "00"}}

		observed := newObservedInstances(
			cluster,
			[]appsv1.StatefulSet{
				{
					ObjectMeta: metav1.ObjectMeta{
						Name: "the-name",
						Labels: map[string]string{
							"postgres-operator.crunchydata.com/instance-set": "00",
						},
					},
				},
			},
			[]corev1.Pod{
				{
					ObjectMeta: metav1.ObjectMeta{
						Name: "some-pod-name",
						Labels: map[string]string{
							"postgres-operator.crunchydata.com/instance-set": "00",
							"postgres-operator.crunchydata.com/instance":     "the-name",
						},
					},
				},
			})

		// Registers as one instance.
		assert.Equal(t, len(observed.forCluster), 1)
		assert.Equal(t, len(observed.byName), 1)
		assert.Equal(t, len(observed.bySet), 1)

		instance := observed.forCluster[0]
		assert.Equal(t, instance.Name, "the-name")
		assert.Equal(t, len(instance.Pods), 1)   // The Pod
		assert.Assert(t, instance.Runner != nil) // The StatefulSet
		assert.Assert(t, instance.Spec != nil)   // The PostgresInstanceSetSpec

		// Lookup based on its name and labels.
		assert.Equal(t, observed.byName["the-name"], instance)
		assert.DeepEqual(t, observed.bySet["00"], []*Instance{instance})
		assert.DeepEqual(t, observed.setNames.List(), []string{"00"})
	})
}

func TestWritablePod(t *testing.T) {
	container := "container"

	t.Run("empty observed", func(t *testing.T) {
		observed := &observedInstances{}

		pod, instance := observed.writablePod("container")
		assert.Assert(t, pod == nil)
		assert.Assert(t, instance == nil)
	})
	t.Run("terminating", func(t *testing.T) {
		instances := []*Instance{
			{
				Name: "instance",
				Pods: []*corev1.Pod{{
					ObjectMeta: metav1.ObjectMeta{
						Namespace: "namespace",
						Name:      "pod",
						Annotations: map[string]string{
							"status": `{"role":"master"}`,
						},
						DeletionTimestamp: &metav1.Time{},
					},
					Status: corev1.PodStatus{
						ContainerStatuses: []corev1.ContainerStatus{{
							Name: container,
							State: corev1.ContainerState{
								Running: new(corev1.ContainerStateRunning),
							},
						}},
					},
				}},
				Runner: &appsv1.StatefulSet{},
			},
		}
		observed := &observedInstances{forCluster: instances}

		terminating, known := observed.forCluster[0].IsTerminating()
		assert.Assert(t, terminating && known)

		pod, instance := observed.writablePod("container")
		assert.Assert(t, pod == nil)
		assert.Assert(t, instance == nil)
	})
	t.Run("not running", func(t *testing.T) {
		instances := []*Instance{
			{
				Name: "instance",
				Pods: []*corev1.Pod{{
					ObjectMeta: metav1.ObjectMeta{
						Namespace: "namespace",
						Name:      "pod",
						Annotations: map[string]string{
							"status": `{"role":"master"}`,
						},
					},
					Status: corev1.PodStatus{
						ContainerStatuses: []corev1.ContainerStatus{{
							Name: container,
							State: corev1.ContainerState{
								Waiting: new(corev1.ContainerStateWaiting)},
						}},
					},
				}},
				Runner: &appsv1.StatefulSet{},
			},
		}
		observed := &observedInstances{forCluster: instances}

		running, known := observed.forCluster[0].IsRunning(container)
		assert.Check(t, !running && known)

		pod, instance := observed.writablePod("container")
		assert.Assert(t, pod == nil)
		assert.Assert(t, instance == nil)
	})
	t.Run("not writable", func(t *testing.T) {
		instances := []*Instance{
			{
				Name: "instance",
				Pods: []*corev1.Pod{{
					ObjectMeta: metav1.ObjectMeta{
						Namespace: "namespace",
						Name:      "pod",
						Annotations: map[string]string{
							"status": `{"role":"replica"}`,
						},
					},
					Status: corev1.PodStatus{
						ContainerStatuses: []corev1.ContainerStatus{{
							Name: container,
							State: corev1.ContainerState{
								Running: new(corev1.ContainerStateRunning),
							},
						}},
					},
				}},
				Runner: &appsv1.StatefulSet{},
			},
		}
		observed := &observedInstances{forCluster: instances}

		writable, known := observed.forCluster[0].IsWritable()
		assert.Check(t, !writable && known)

		pod, instance := observed.writablePod("container")
		assert.Assert(t, pod == nil)
		assert.Assert(t, instance == nil)
	})
	t.Run("writable instance exists", func(t *testing.T) {
		instances := []*Instance{
			{
				Name: "instance",
				Pods: []*corev1.Pod{{
					ObjectMeta: metav1.ObjectMeta{
						Namespace: "namespace",
						Name:      "pod",
						Annotations: map[string]string{
							"status": `{"role":"master"}`,
						},
					},
					Status: corev1.PodStatus{
						ContainerStatuses: []corev1.ContainerStatus{{
							Name: container,
							State: corev1.ContainerState{
								Running: new(corev1.ContainerStateRunning),
							},
						}},
					},
				}},
				Runner: &appsv1.StatefulSet{},
			},
		}
		observed := &observedInstances{forCluster: instances}

		terminating, known := observed.forCluster[0].IsTerminating()
		assert.Check(t, !terminating && known)
		writable, known := observed.forCluster[0].IsWritable()
		assert.Check(t, writable && known)
		running, known := observed.forCluster[0].IsRunning(container)
		assert.Check(t, running && known)

		pod, instance := observed.writablePod("container")
		assert.Assert(t, pod != nil)
		assert.Assert(t, instance != nil)
	})
}

func TestAddPGBackRestToInstancePodSpec(t *testing.T) {

	clusterName := "hippo"
	clusterUID := types.UID("hippouid")
	namespace := "test-add-pgbackrest-to-instance-pod-spec"

	// create a PostgresCluster to test with
	postgresCluster := &v1beta1.PostgresCluster{
		ObjectMeta: metav1.ObjectMeta{
			Name:      clusterName,
			Namespace: namespace,
			UID:       clusterUID,
		},
		Spec: v1beta1.PostgresClusterSpec{
			Backups: v1beta1.Backups{
				PGBackRest: v1beta1.PGBackRestArchive{
					Repos: []v1beta1.PGBackRestRepo{{
						Name: "repo1",
						Volume: &v1beta1.RepoPVC{
							VolumeClaimSpec: corev1.PersistentVolumeClaimSpec{
								AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteMany},
								Resources: corev1.ResourceRequirements{
									Requests: map[corev1.ResourceName]resource.Quantity{
										corev1.ResourceStorage: resource.MustParse("1Gi"),
									},
								},
							},
						},
					}, {
						Name: "repo2",
						Volume: &v1beta1.RepoPVC{
							VolumeClaimSpec: corev1.PersistentVolumeClaimSpec{
								AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteMany},
								Resources: corev1.ResourceRequirements{
									Requests: map[corev1.ResourceName]resource.Quantity{
										corev1.ResourceStorage: resource.MustParse("2Gi"),
									},
								},
							},
						},
					}},
				},
			},
		},
	}

	testCases := []struct {
		dedicatedRepoHostEnabled bool
		sshConfig                *corev1.ConfigMapProjection
		sshSecret                *corev1.SecretProjection
	}{{
		dedicatedRepoHostEnabled: false,
	}, {
		dedicatedRepoHostEnabled: true,
		sshConfig: &corev1.ConfigMapProjection{
			LocalObjectReference: corev1.LocalObjectReference{Name: "cust-ssh-config.conf"}},
		sshSecret: &corev1.SecretProjection{
			LocalObjectReference: corev1.LocalObjectReference{Name: "cust-ssh-secret.conf"}},
	}}

	for _, tc := range testCases {
		dedicated := tc.dedicatedRepoHostEnabled
		customConfig := (tc.sshConfig != nil)
		customSecret := (tc.sshSecret != nil)
		t.Run(fmt.Sprintf("dedicated:%t", dedicated), func(t *testing.T) {

			template := &corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{{Name: naming.ContainerDatabase}},
				},
			}

			pgBackRestConfigContainers := []string{naming.ContainerDatabase}
			if dedicated {
				pgBackRestConfigContainers = append(pgBackRestConfigContainers,
					naming.PGBackRestRepoContainerName)
				if customConfig || customSecret {
					if postgresCluster.Spec.Backups.PGBackRest.RepoHost == nil {
						postgresCluster.Spec.Backups.PGBackRest.RepoHost = &v1beta1.PGBackRestRepoHost{}
					}
					postgresCluster.Spec.Backups.PGBackRest.RepoHost.SSHConfiguration = tc.sshConfig
					postgresCluster.Spec.Backups.PGBackRest.RepoHost.SSHSecret = tc.sshSecret
				}
			}

			err := addPGBackRestToInstancePodSpec(postgresCluster, template)
			assert.NilError(t, err)

			// if a repo host is configured, then verify SSH is enabled
			if dedicated {

				// verify the ssh volume
				var foundSSHVolume bool
				var sshVolume corev1.Volume
				for _, v := range template.Spec.Volumes {
					if v.Name == naming.PGBackRestSSHVolume {
						foundSSHVolume = true
						sshVolume = v
						break
					}
				}
				assert.Assert(t, foundSSHVolume)

				// verify the ssh config and secret
				var foundSSHConfigVolume, foundSSHSecretVolume bool
				defaultConfigName := naming.PGBackRestSSHConfig(postgresCluster).Name
				defaultSecretName := naming.PGBackRestSSHSecret(postgresCluster).Name
				for _, s := range sshVolume.Projected.Sources {
					if s.ConfigMap != nil {
						if (!customConfig && s.ConfigMap.Name == defaultConfigName) ||
							(customConfig && s.ConfigMap.Name == tc.sshConfig.Name) {
							foundSSHConfigVolume = true
						}
					} else if s.Secret != nil {
						if (!customSecret && s.Secret.Name == defaultSecretName) ||
							(customSecret && s.Secret.Name == tc.sshSecret.Name) {
							foundSSHSecretVolume = true
						}
					}
				}
				assert.Assert(t, foundSSHConfigVolume)
				assert.Assert(t, foundSSHSecretVolume)

				// verify that pgbackrest container is present and that the proper SSH volume mount in
				// present in all containers
				var foundSSHContainer bool
				for _, c := range template.Spec.Containers {
					if c.Name == naming.PGBackRestRepoContainerName {
						foundSSHContainer = true
					}
					var foundVolumeMount bool
					for _, vm := range c.VolumeMounts {
						if vm.Name == naming.PGBackRestSSHVolume && vm.MountPath == "/etc/ssh" &&
							vm.ReadOnly == true {
							foundVolumeMount = true
							break
						}
					}
					assert.Assert(t, foundVolumeMount)
				}
				assert.Assert(t, foundSSHContainer)
			}

			var foundConfigVolume bool
			var configVolume corev1.Volume
			for _, v := range template.Spec.Volumes {
				if v.Name == pgbackrest.ConfigVol {
					foundConfigVolume = true
					configVolume = v
					break
				}
			}
			assert.Assert(t, foundConfigVolume)

			var foundConfigProjection bool
			defaultConfigName := naming.PGBackRestConfig(postgresCluster).Name
			for _, s := range configVolume.Projected.Sources {
				if s.ConfigMap != nil {
					if s.ConfigMap.Name == defaultConfigName {
						foundConfigProjection = true
					}
				}
			}
			assert.Assert(t, foundConfigProjection)

			for _, container := range pgBackRestConfigContainers {
				var foundContainer bool
				for _, c := range template.Spec.Containers {
					if c.Name == container {
						foundContainer = true
					}
					var foundVolumeMount bool
					for _, vm := range c.VolumeMounts {
						if vm.Name == pgbackrest.ConfigVol && vm.MountPath == pgbackrest.ConfigDir {
							foundVolumeMount = true
							break
						}
					}
					assert.Assert(t, foundVolumeMount)
				}
				assert.Assert(t, foundContainer)
			}
		})
	}
}

func TestPodsToKeep(t *testing.T) {
	for _, test := range []struct {
		name      string
		instances []corev1.Pod
		want      map[string]int
		checks    func(*testing.T, []corev1.Pod)
	}{
		{
			name: "RemoveSetWithMasterOnly",
			instances: []corev1.Pod{
				{
					ObjectMeta: metav1.ObjectMeta{
						Name: "daisy-asdf",
						Labels: map[string]string{
							naming.LabelRole:        "master",
							naming.LabelInstanceSet: "daisy",
						},
					},
				},
			},
			want: map[string]int{},
			checks: func(t *testing.T, p []corev1.Pod) {
				assert.Equal(t, len(p), 0)
			},
		}, {
			name: "RemoveSetWithReplicaOnly",
			instances: []corev1.Pod{
				{
					ObjectMeta: metav1.ObjectMeta{
						Name: "daisy-asdf",
						Labels: map[string]string{
							naming.LabelRole:        "replica",
							naming.LabelInstanceSet: "daisy",
						},
					},
				},
			},
			want: map[string]int{},
			checks: func(t *testing.T, p []corev1.Pod) {
				assert.Equal(t, len(p), 0)
			},
		}, {
			name: "KeepMasterOnly",
			instances: []corev1.Pod{
				{
					ObjectMeta: metav1.ObjectMeta{
						Name: "daisy-asdf",
						Labels: map[string]string{
							naming.LabelRole:        "master",
							naming.LabelInstanceSet: "daisy",
						},
					},
				},
			},
			want: map[string]int{
				"daisy": 1,
			},
			checks: func(t *testing.T, p []corev1.Pod) {
				assert.Equal(t, len(p), 1)
			},
		}, {
			name: "KeepNoRoleLabels",
			instances: []corev1.Pod{
				{
					ObjectMeta: metav1.ObjectMeta{
						Name: "daisy-asdf",
						Labels: map[string]string{
							naming.LabelInstanceSet: "daisy",
						},
					},
				},
			},
			want: map[string]int{
				"daisy": 1,
			},
			checks: func(t *testing.T, p []corev1.Pod) {
				assert.Equal(t, len(p), 1)
			},
		}, {
			name: "RemoveSetWithNoRoleLabels",
			instances: []corev1.Pod{
				{
					ObjectMeta: metav1.ObjectMeta{
						Name: "daisy-asdf",
						Labels: map[string]string{
							naming.LabelInstanceSet: "daisy",
						},
					},
				},
			},
			want: map[string]int{},
			checks: func(t *testing.T, p []corev1.Pod) {
				assert.Equal(t, len(p), 0)
			},
		}, {
			name: "KeepUnknownRoleLabel",
			instances: []corev1.Pod{
				{
					ObjectMeta: metav1.ObjectMeta{
						Name: "daisy-asdf",
						Labels: map[string]string{
							naming.LabelRole:        "unknownLabelRole",
							naming.LabelInstanceSet: "daisy",
						},
					},
				},
			},
			want: map[string]int{
				"daisy": 1,
			},
			checks: func(t *testing.T, p []corev1.Pod) {
				assert.Equal(t, len(p), 1)
			},
		}, {
			name: "RemoveSetWithUnknownRoleLabel",
			instances: []corev1.Pod{
				{
					ObjectMeta: metav1.ObjectMeta{
						Name: "daisy-asdf",
						Labels: map[string]string{
							naming.LabelRole:        "unknownLabelRole",
							naming.LabelInstanceSet: "daisy",
						},
					},
				},
			},
			want: map[string]int{},
			checks: func(t *testing.T, p []corev1.Pod) {
				assert.Equal(t, len(p), 0)
			},
		}, {
			name: "MasterLastInSet",
			instances: []corev1.Pod{
				{
					ObjectMeta: metav1.ObjectMeta{
						Name: "daisy-asdf",
						Labels: map[string]string{
							naming.LabelRole:        "replica",
							naming.LabelInstanceSet: "daisy",
						},
					},
				},
				{
					ObjectMeta: metav1.ObjectMeta{
						Name: "daisy-poih",
						Labels: map[string]string{
							naming.LabelRole:        "master",
							naming.LabelInstanceSet: "daisy",
						},
					},
				},
			},
			want: map[string]int{
				"daisy": 1,
			},
			checks: func(t *testing.T, p []corev1.Pod) {
				assert.Equal(t, len(p), 1)
				assert.Equal(t, p[0].Labels[naming.LabelRole], "master")
			},
		}, {
			name: "ScaleDownSetWithMaster",
			instances: []corev1.Pod{
				{
					ObjectMeta: metav1.ObjectMeta{
						Name: "max-asdf",
						Labels: map[string]string{
							naming.LabelRole:        "replica",
							naming.LabelInstanceSet: "max",
						},
					},
				},
				{
					ObjectMeta: metav1.ObjectMeta{
						Name: "daisy-poih",
						Labels: map[string]string{
							naming.LabelRole:        "master",
							naming.LabelInstanceSet: "daisy",
						},
					},
				},
				{
					ObjectMeta: metav1.ObjectMeta{
						Name: "daisy-dogs",
						Labels: map[string]string{
							naming.LabelRole:        "replica",
							naming.LabelInstanceSet: "daisy",
						},
					},
				},
				{
					ObjectMeta: metav1.ObjectMeta{
						Name: "max-dogs",
						Labels: map[string]string{
							naming.LabelRole:        "replica",
							naming.LabelInstanceSet: "daisy",
						},
					},
				},
			},
			want: map[string]int{
				"max":   1,
				"daisy": 1,
			},
			checks: func(t *testing.T, p []corev1.Pod) {
				assert.Equal(t, len(p), 2)
				assert.Equal(t, p[0].Labels[naming.LabelRole], "master")
				assert.Equal(t, p[0].Labels[naming.LabelInstanceSet], "daisy")
				assert.Equal(t, p[1].Labels[naming.LabelRole], "replica")
				assert.Equal(t, p[1].Labels[naming.LabelInstanceSet], "max")
			},
		}, {
			name: "ScaleDownSetWithoutMaster",
			instances: []corev1.Pod{
				{
					ObjectMeta: metav1.ObjectMeta{
						Name: "max-asdf",
						Labels: map[string]string{
							naming.LabelRole:        "master",
							naming.LabelInstanceSet: "max",
						},
					},
				},
				{
					ObjectMeta: metav1.ObjectMeta{
						Name: "daisy-poih",
						Labels: map[string]string{
							naming.LabelRole:        "replica",
							naming.LabelInstanceSet: "daisy",
						},
					},
				},
				{
					ObjectMeta: metav1.ObjectMeta{
						Name: "daisy-dogs",
						Labels: map[string]string{
							naming.LabelRole:        "replica",
							naming.LabelInstanceSet: "daisy",
						},
					},
				},
				{
					ObjectMeta: metav1.ObjectMeta{
						Name: "max-dogs",
						Labels: map[string]string{
							naming.LabelRole:        "replica",
							naming.LabelInstanceSet: "daisy",
						},
					},
				},
			},
			want: map[string]int{
				"max":   1,
				"daisy": 2,
			},
			checks: func(t *testing.T, p []corev1.Pod) {
				assert.Equal(t, len(p), 3)
				assert.Equal(t, p[0].Labels[naming.LabelRole], "master")
				assert.Equal(t, p[0].Labels[naming.LabelInstanceSet], "max")
				assert.Equal(t, p[1].Labels[naming.LabelInstanceSet], "daisy")
				assert.Equal(t, p[1].Labels[naming.LabelRole], "replica")
				assert.Equal(t, p[2].Labels[naming.LabelInstanceSet], "daisy")
				assert.Equal(t, p[2].Labels[naming.LabelRole], "replica")
			},
		}, {
			name: "ScaleMasterSetToZero",
			instances: []corev1.Pod{
				{
					ObjectMeta: metav1.ObjectMeta{
						Name: "max-asdf",
						Labels: map[string]string{
							naming.LabelRole:        "master",
							naming.LabelInstanceSet: "max",
						},
					},
				},
				{
					ObjectMeta: metav1.ObjectMeta{
						Name: "daisy-poih",
						Labels: map[string]string{
							naming.LabelRole:        "replica",
							naming.LabelInstanceSet: "daisy",
						},
					},
				},
				{
					ObjectMeta: metav1.ObjectMeta{
						Name: "daisy-dogs",
						Labels: map[string]string{
							naming.LabelRole:        "replica",
							naming.LabelInstanceSet: "daisy",
						},
					},
				},
			},
			want: map[string]int{
				"max":   0,
				"daisy": 2,
			},
			checks: func(t *testing.T, p []corev1.Pod) {
				assert.Equal(t, len(p), 2)
				assert.Equal(t, p[0].Labels[naming.LabelRole], "replica")
				assert.Equal(t, p[0].Labels[naming.LabelInstanceSet], "daisy")
				assert.Equal(t, p[1].Labels[naming.LabelRole], "replica")
				assert.Equal(t, p[1].Labels[naming.LabelInstanceSet], "daisy")
			},
		}, {
			name: "RemoveMasterInstanceSet",
			instances: []corev1.Pod{
				{
					ObjectMeta: metav1.ObjectMeta{
						Name: "max-asdf",
						Labels: map[string]string{
							naming.LabelRole:        "master",
							naming.LabelInstanceSet: "max",
						},
					},
				},
				{
					ObjectMeta: metav1.ObjectMeta{
						Name: "daisy-poih",
						Labels: map[string]string{
							naming.LabelRole:        "replica",
							naming.LabelInstanceSet: "daisy",
						},
					},
				},
				{
					ObjectMeta: metav1.ObjectMeta{
						Name: "daisy-dogs",
						Labels: map[string]string{
							naming.LabelRole:        "replica",
							naming.LabelInstanceSet: "daisy",
						},
					},
				},
				{
					ObjectMeta: metav1.ObjectMeta{
						Name: "max-dogs",
						Labels: map[string]string{
							naming.LabelRole:        "replica",
							naming.LabelInstanceSet: "daisy",
						},
					},
				},
			},
			want: map[string]int{
				"daisy": 3,
			},
			checks: func(t *testing.T, p []corev1.Pod) {
				assert.Equal(t, len(p), 3)
				assert.Equal(t, p[0].Labels[naming.LabelRole], "replica")
				assert.Equal(t, p[0].Labels[naming.LabelInstanceSet], "daisy")
				assert.Equal(t, p[1].Labels[naming.LabelRole], "replica")
				assert.Equal(t, p[1].Labels[naming.LabelInstanceSet], "daisy")
				assert.Equal(t, p[2].Labels[naming.LabelRole], "replica")
				assert.Equal(t, p[2].Labels[naming.LabelInstanceSet], "daisy")
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			keep := podsToKeep(test.instances, test.want)
			sort.Slice(keep, func(i, j int) bool {
				return keep[i].Labels[naming.LabelRole] == "master"
			})
			test.checks(t, keep)
		})
	}
}

func TestDeleteInstance(t *testing.T) {
	env, cc, config := setupTestEnv(t, ControllerName)
	t.Cleanup(func() { teardownTestEnv(t, env) })

	reconciler := &Reconciler{}
	ctx, cancel := setupManager(t, config, func(mgr manager.Manager) {
		reconciler = &Reconciler{
			Client:   cc,
			Owner:    client.FieldOwner(t.Name()),
			Recorder: mgr.GetEventRecorderFor(ControllerName),
			Tracer:   otel.Tracer(t.Name()),
		}
	})
	t.Cleanup(func() { teardownManager(cancel, t) })

	ns := &corev1.Namespace{}
	ns.GenerateName = "postgres-operator-test-"
	ns.Labels = labels.Set{"postgres-operator-test": t.Name()}
	assert.NilError(t, reconciler.Client.Create(ctx, ns))
	t.Cleanup(func() { assert.Check(t, reconciler.Client.Delete(ctx, ns)) })

	// Define, Create, and Reconcile a cluster to get an instance running in kube
	cluster := testCluster()
	cluster.Namespace = ns.Name

	assert.NilError(t, errors.WithStack(reconciler.Client.Create(ctx, cluster)))
	t.Cleanup(func() {
		// Remove finalizers, if any, so the namespace can terminate.
		assert.Check(t, client.IgnoreNotFound(
			reconciler.Client.Patch(ctx, cluster, client.RawPatch(
				client.Merge.Type(), []byte(`{"metadata":{"finalizers":[]}}`)))))
	})

	// Reconcile the entire cluster so that we don't have to create all the
	// resources needed to reconcile a single instance (cm,secrets,svc, etc.)
	result, err := reconciler.Reconcile(ctx, reconcile.Request{
		NamespacedName: client.ObjectKeyFromObject(cluster),
	})
	assert.NilError(t, err)
	assert.Assert(t, result.Requeue == false)

	stsList := &appsv1.StatefulSetList{}
	assert.NilError(t, reconciler.Client.List(ctx, stsList,
		client.InNamespace(cluster.Namespace),
		client.MatchingLabels{
			naming.LabelCluster:     cluster.Name,
			naming.LabelInstanceSet: cluster.Spec.InstanceSets[0].Name,
		}))

	// Grab the instance name off of the instance set at index0
	instanceName := stsList.Items[0].Labels[naming.LabelInstance]

	// Use the instance name to delete the single instance
	assert.NilError(t, reconciler.deleteInstance(ctx, cluster, instanceName))

	gvks := []schema.GroupVersionKind{
		corev1.SchemeGroupVersion.WithKind("PersistentVolumeClaim"),
		corev1.SchemeGroupVersion.WithKind("ConfigMap"),
		corev1.SchemeGroupVersion.WithKind("Secret"),
		appsv1.SchemeGroupVersion.WithKind("StatefulSet"),
	}

	selector, err := naming.AsSelector(metav1.LabelSelector{
		MatchLabels: map[string]string{
			naming.LabelCluster:  cluster.Name,
			naming.LabelInstance: instanceName,
		}})
	assert.NilError(t, err)

	for _, gvk := range gvks {
		t.Run(gvk.Kind, func(t *testing.T) {
			uList := &unstructured.UnstructuredList{}
			err := wait.Poll(time.Second*3, Scale(time.Second*30), func() (bool, error) {
				uList.SetGroupVersionKind(gvk)
				assert.NilError(t, errors.WithStack(reconciler.Client.List(ctx, uList,
					client.InNamespace(cluster.Namespace),
					client.MatchingLabelsSelector{Selector: selector})))

				if len(uList.Items) == 0 {
					return true, nil
				}

				// Check existing objects for deletionTimestamp ensuring they
				// are staged for delete
				deleted := true
				for i := range uList.Items {
					u := uList.Items[i]
					if u.GetDeletionTimestamp() == nil {
						deleted = false
					}
				}

				// We have found objects that are not staged for delete
				// so deleteInstance has failed
				return deleted, nil
			})
			assert.NilError(t, err)
		})
	}
}

func TestGenerateInstanceStatefulSetIntent(t *testing.T) {
	type intentParams struct {
		cluster                    *v1beta1.PostgresCluster
		spec                       *v1beta1.PostgresInstanceSetSpec
		clusterPodServiceName      string
		instanceServiceAccountName string
		sts                        *appsv1.StatefulSet
		shutdown                   bool
		startupInstance            string
		numInstancePods            int
	}

	for _, test := range []struct {
		name string
		ip   intentParams
		run  func(*testing.T, *appsv1.StatefulSet)
	}{{
		name: "cluster pod service name",
		ip: intentParams{
			clusterPodServiceName: "daisy-svc",
		},
		run: func(t *testing.T, ss *appsv1.StatefulSet) {
			assert.Equal(t, ss.Spec.ServiceName, "daisy-svc")
		},
	}, {
		name: "instance service account name",
		ip: intentParams{
			instanceServiceAccountName: "daisy-sa",
		},
		run: func(t *testing.T, ss *appsv1.StatefulSet) {
			assert.Equal(t, ss.Spec.Template.Spec.ServiceAccountName, "daisy-sa")
		},
	}, {
		name: "custom affinity",
		ip: intentParams{
			spec: &v1beta1.PostgresInstanceSetSpec{
				Affinity: &corev1.Affinity{},
			},
		},
		run: func(t *testing.T, ss *appsv1.StatefulSet) {
			assert.Assert(t, ss.Spec.Template.Spec.Affinity != nil)
		},
	}, {
		name: "custom tolerations",
		ip: intentParams{
			spec: &v1beta1.PostgresInstanceSetSpec{
				Tolerations: []corev1.Toleration{},
			},
		},
		run: func(t *testing.T, ss *appsv1.StatefulSet) {
			assert.Assert(t, ss.Spec.Template.Spec.Tolerations != nil)
		},
	}, {
		name: "custom topology spread constraints",
		ip: intentParams{
			spec: &v1beta1.PostgresInstanceSetSpec{
				TopologySpreadConstraints: []corev1.TopologySpreadConstraint{},
			},
		},
		run: func(t *testing.T, ss *appsv1.StatefulSet) {
			assert.Assert(t, ss.Spec.Template.Spec.TopologySpreadConstraints != nil)
		},
	}, {
		name: "shutdown replica",
		ip: intentParams{
			shutdown:        true,
			numInstancePods: 2,
			startupInstance: "testInstance1",
		},
		run: func(t *testing.T, ss *appsv1.StatefulSet) {
			assert.Equal(t, *ss.Spec.Replicas, int32(0))
		},
	}, {
		name: "shutdown primary",
		ip: intentParams{
			shutdown:        true,
			numInstancePods: 1,
			startupInstance: "testInstance1",
		},
		run: func(t *testing.T, ss *appsv1.StatefulSet) {
			assert.Equal(t, *ss.Spec.Replicas, int32(0))
		},
	}, {
		name: "startup primary",
		ip: intentParams{
			shutdown:        false,
			numInstancePods: 0,
		},
		run: func(t *testing.T, ss *appsv1.StatefulSet) {
			assert.Equal(t, *ss.Spec.Replicas, int32(1))
		},
	}, {
		name: "startup replica",
		ip: intentParams{
			shutdown:        false,
			numInstancePods: 1,
		},
		run: func(t *testing.T, ss *appsv1.StatefulSet) {
			assert.Equal(t, *ss.Spec.Replicas, int32(1))
		},
	}, {
		name: "do not startup replica",
		ip: intentParams{
			shutdown:        false,
			numInstancePods: 0,
			startupInstance: "testInstance1",
		},
		run: func(t *testing.T, ss *appsv1.StatefulSet) {
			assert.Equal(t, *ss.Spec.Replicas, int32(0))
		},
	}, {
		name: "do not shutdown primary",
		ip: intentParams{
			shutdown:        true,
			numInstancePods: 2,
		},
		run: func(t *testing.T, ss *appsv1.StatefulSet) {
			assert.Equal(t, *ss.Spec.Replicas, int32(1))
		},
	}, {
		name: "check imagepullsecret",
		run: func(t *testing.T, ss *appsv1.StatefulSet) {
			assert.Assert(t, ss.Spec.Template.Spec.ImagePullSecrets != nil)
			assert.Equal(t, ss.Spec.Template.Spec.ImagePullSecrets[0].Name,
				"myImagePullSecret")
		},
	}, {
		name: "check pod priority",
		ip: intentParams{
			spec: &v1beta1.PostgresInstanceSetSpec{
				PriorityClassName: initialize.String("some-priority-class"),
			},
		},
		run: func(t *testing.T, ss *appsv1.StatefulSet) {
			assert.Equal(t, ss.Spec.Template.Spec.PriorityClassName,
				"some-priority-class")
		},
	}, {
		name: "check default scheduling constraints are added",
		run: func(t *testing.T, ss *appsv1.StatefulSet) {
			assert.Equal(t, len(ss.Spec.Template.Spec.TopologySpreadConstraints), 2)
			assert.Assert(t, marshalMatches(ss.Spec.Template.Spec.TopologySpreadConstraints, `
- labelSelector:
    matchExpressions:
    - key: postgres-operator.crunchydata.com/data
      operator: In
      values:
      - postgres
      - pgbackrest
    matchLabels:
      postgres-operator.crunchydata.com/cluster: hippo
  maxSkew: 1
  topologyKey: kubernetes.io/hostname
  whenUnsatisfiable: ScheduleAnyway
- labelSelector:
    matchExpressions:
    - key: postgres-operator.crunchydata.com/data
      operator: In
      values:
      - postgres
      - pgbackrest
    matchLabels:
      postgres-operator.crunchydata.com/cluster: hippo
  maxSkew: 1
  topologyKey: topology.kubernetes.io/zone
  whenUnsatisfiable: ScheduleAnyway
			`))
		},
	}, {
		name: "check default scheduling constraints are appended to existing",
		ip: intentParams{
			spec: &v1beta1.PostgresInstanceSetSpec{
				Name: "instance1",
				TopologySpreadConstraints: []corev1.TopologySpreadConstraint{{
					MaxSkew:           int32(1),
					TopologyKey:       "kubernetes.io/hostname",
					WhenUnsatisfiable: corev1.ScheduleAnyway,
					LabelSelector: &metav1.LabelSelector{
						MatchExpressions: []metav1.LabelSelectorRequirement{
							{Key: naming.LabelCluster, Operator: "In", Values: []string{"somename"}},
							{Key: naming.LabelData, Operator: "Exists"},
						},
					},
				}},
			},
		},
		run: func(t *testing.T, ss *appsv1.StatefulSet) {
			assert.Equal(t, len(ss.Spec.Template.Spec.TopologySpreadConstraints), 3)
			assert.Assert(t, marshalMatches(ss.Spec.Template.Spec.TopologySpreadConstraints, `
- labelSelector:
    matchExpressions:
    - key: postgres-operator.crunchydata.com/cluster
      operator: In
      values:
      - somename
    - key: postgres-operator.crunchydata.com/data
      operator: Exists
  maxSkew: 1
  topologyKey: kubernetes.io/hostname
  whenUnsatisfiable: ScheduleAnyway
- labelSelector:
    matchExpressions:
    - key: postgres-operator.crunchydata.com/data
      operator: In
      values:
      - postgres
      - pgbackrest
    matchLabels:
      postgres-operator.crunchydata.com/cluster: hippo
  maxSkew: 1
  topologyKey: kubernetes.io/hostname
  whenUnsatisfiable: ScheduleAnyway
- labelSelector:
    matchExpressions:
    - key: postgres-operator.crunchydata.com/data
      operator: In
      values:
      - postgres
      - pgbackrest
    matchLabels:
      postgres-operator.crunchydata.com/cluster: hippo
  maxSkew: 1
  topologyKey: topology.kubernetes.io/zone
  whenUnsatisfiable: ScheduleAnyway
			`))
		},
	}, {
		name: "check defined constraint when defaults disabled",
		ip: intentParams{
			cluster: &v1beta1.PostgresCluster{
				ObjectMeta: metav1.ObjectMeta{
					Name: "hippo",
				},
				Spec: v1beta1.PostgresClusterSpec{
					PostgresVersion:             13,
					DisableDefaultPodScheduling: initialize.Bool(true),
					InstanceSets: []v1beta1.PostgresInstanceSetSpec{{
						Name:                "instance1",
						Replicas:            Int32(1),
						DataVolumeClaimSpec: testVolumeClaimSpec(),
						TopologySpreadConstraints: []corev1.TopologySpreadConstraint{{
							MaxSkew:           int32(1),
							TopologyKey:       "kubernetes.io/hostname",
							WhenUnsatisfiable: corev1.ScheduleAnyway,
							LabelSelector: &metav1.LabelSelector{
								MatchExpressions: []metav1.LabelSelectorRequirement{
									{Key: naming.LabelCluster, Operator: "In", Values: []string{"somename"}},
									{Key: naming.LabelData, Operator: "Exists"},
								},
							},
						}},
					}},
				},
			},
			spec: &v1beta1.PostgresInstanceSetSpec{
				Name: "instance1",
				TopologySpreadConstraints: []corev1.TopologySpreadConstraint{{
					MaxSkew:           int32(1),
					TopologyKey:       "kubernetes.io/hostname",
					WhenUnsatisfiable: corev1.ScheduleAnyway,
					LabelSelector: &metav1.LabelSelector{
						MatchExpressions: []metav1.LabelSelectorRequirement{
							{Key: naming.LabelCluster, Operator: "In", Values: []string{"somename"}},
							{Key: naming.LabelData, Operator: "Exists"},
						},
					},
				}},
			},
		},
		run: func(t *testing.T, ss *appsv1.StatefulSet) {
			assert.Equal(t, len(ss.Spec.Template.Spec.TopologySpreadConstraints), 1)
			assert.Assert(t, marshalMatches(ss.Spec.Template.Spec.TopologySpreadConstraints,
				`- labelSelector:
    matchExpressions:
    - key: postgres-operator.crunchydata.com/cluster
      operator: In
      values:
      - somename
    - key: postgres-operator.crunchydata.com/data
      operator: Exists
  maxSkew: 1
  topologyKey: kubernetes.io/hostname
  whenUnsatisfiable: ScheduleAnyway
`))
		},
	}} {
		t.Run(test.name, func(t *testing.T) {

			cluster := test.ip.cluster
			if cluster == nil {
				cluster = testCluster()
			}

			cluster.Default()
			cluster.UID = types.UID("hippouid")
			cluster.Namespace = test.name + "-ns"
			cluster.Spec.Shutdown = &test.ip.shutdown
			cluster.Status.StartupInstance = test.ip.startupInstance

			spec := test.ip.spec
			if spec == nil {
				spec = new(v1beta1.PostgresInstanceSetSpec)
				spec.Default(0)
			}

			clusterPodServiceName := test.ip.clusterPodServiceName
			instanceServiceAccountName := test.ip.instanceServiceAccountName
			sts := test.ip.sts
			if sts == nil {
				sts = &appsv1.StatefulSet{}
			}

			generateInstanceStatefulSetIntent(context.Background(),
				cluster, spec,
				clusterPodServiceName,
				instanceServiceAccountName,
				sts,
				test.ip.numInstancePods,
			)

			test.run(t, sts)
		})
	}
}

func TestFindAvailableInstanceNames(t *testing.T) {

	testCases := []struct {
		set                   v1beta1.PostgresInstanceSetSpec
		fakeObservedInstances *observedInstances
		fakeClusterVolumes    []corev1.PersistentVolumeClaim
		expectedInstanceNames []string
	}{{
		set: v1beta1.PostgresInstanceSetSpec{Name: "instance1"},
		fakeObservedInstances: newObservedInstances(
			&v1beta1.PostgresCluster{Spec: v1beta1.PostgresClusterSpec{
				InstanceSets: []v1beta1.PostgresInstanceSetSpec{{}},
			}},
			[]appsv1.StatefulSet{{}},
			[]corev1.Pod{},
		),
		fakeClusterVolumes:    []corev1.PersistentVolumeClaim{{}},
		expectedInstanceNames: []string{},
	}, {
		set: v1beta1.PostgresInstanceSetSpec{Name: "instance1"},
		fakeObservedInstances: newObservedInstances(
			&v1beta1.PostgresCluster{Spec: v1beta1.PostgresClusterSpec{
				InstanceSets: []v1beta1.PostgresInstanceSetSpec{{Name: "instance1"}},
			}},
			[]appsv1.StatefulSet{{ObjectMeta: metav1.ObjectMeta{
				Name: "instance1-abc",
				Labels: map[string]string{
					naming.LabelInstanceSet: "instance1"}}}},
			[]corev1.Pod{},
		),
		fakeClusterVolumes: []corev1.PersistentVolumeClaim{{ObjectMeta: metav1.ObjectMeta{
			Name: "instance1-abc-def",
			Labels: map[string]string{
				naming.LabelRole:        naming.RolePostgresData,
				naming.LabelInstanceSet: "instance1",
				naming.LabelInstance:    "instance1-abc"}}}},
		expectedInstanceNames: []string{},
	}, {
		set: v1beta1.PostgresInstanceSetSpec{Name: "instance1"},
		fakeObservedInstances: newObservedInstances(
			&v1beta1.PostgresCluster{Spec: v1beta1.PostgresClusterSpec{
				InstanceSets: []v1beta1.PostgresInstanceSetSpec{{Name: "instance1"}},
			}},
			[]appsv1.StatefulSet{{ObjectMeta: metav1.ObjectMeta{
				Name: "instance1-abc",
				Labels: map[string]string{
					naming.LabelInstanceSet: "instance1"}}}},
			[]corev1.Pod{},
		),
		fakeClusterVolumes:    []corev1.PersistentVolumeClaim{},
		expectedInstanceNames: []string{},
	}, {
		set: v1beta1.PostgresInstanceSetSpec{Name: "instance1"},
		fakeObservedInstances: newObservedInstances(
			&v1beta1.PostgresCluster{Spec: v1beta1.PostgresClusterSpec{
				InstanceSets: []v1beta1.PostgresInstanceSetSpec{{Name: "instance1"}},
			}},
			[]appsv1.StatefulSet{{ObjectMeta: metav1.ObjectMeta{
				Name: "instance1-abc",
				Labels: map[string]string{
					naming.LabelInstanceSet: "instance1"}}}},
			[]corev1.Pod{},
		),
		fakeClusterVolumes: []corev1.PersistentVolumeClaim{
			{ObjectMeta: metav1.ObjectMeta{
				Name: "instance1-abc-def",
				Labels: map[string]string{
					naming.LabelRole:        naming.RolePostgresData,
					naming.LabelInstanceSet: "instance1",
					naming.LabelInstance:    "instance1-abc"}}},
			{ObjectMeta: metav1.ObjectMeta{
				Name: "instance1-abc-efg",
				Labels: map[string]string{
					naming.LabelRole:        naming.RolePostgresData,
					naming.LabelInstanceSet: "instance1",
					naming.LabelInstance:    "instance1-def"}}},
		},
		expectedInstanceNames: []string{"instance1-def"},
	}, {
		set: v1beta1.PostgresInstanceSetSpec{Name: "instance1"},
		fakeObservedInstances: newObservedInstances(
			&v1beta1.PostgresCluster{Spec: v1beta1.PostgresClusterSpec{
				InstanceSets: []v1beta1.PostgresInstanceSetSpec{{Name: "instance1"}},
			}},
			[]appsv1.StatefulSet{{ObjectMeta: metav1.ObjectMeta{
				Name: "instance1-abc",
				Labels: map[string]string{
					naming.LabelInstanceSet: "instance1"}}}},
			[]corev1.Pod{},
		),
		fakeClusterVolumes: []corev1.PersistentVolumeClaim{{ObjectMeta: metav1.ObjectMeta{
			Name: "instance1-abc-def",
			Labels: map[string]string{
				naming.LabelRole:        naming.RolePostgresData,
				naming.LabelInstanceSet: "instance1",
				naming.LabelInstance:    "instance1-def"}}}},
		expectedInstanceNames: []string{"instance1-def"},
	}, {
		set: v1beta1.PostgresInstanceSetSpec{Name: "instance1",
			WALVolumeClaimSpec: &corev1.PersistentVolumeClaimSpec{}},
		fakeObservedInstances: newObservedInstances(
			&v1beta1.PostgresCluster{Spec: v1beta1.PostgresClusterSpec{
				InstanceSets: []v1beta1.PostgresInstanceSetSpec{{Name: "instance1"}},
			}},
			[]appsv1.StatefulSet{{ObjectMeta: metav1.ObjectMeta{
				Name: "instance1-abc",
				Labels: map[string]string{
					naming.LabelInstanceSet: "instance1"}}}},
			[]corev1.Pod{},
		),
		fakeClusterVolumes: []corev1.PersistentVolumeClaim{
			{ObjectMeta: metav1.ObjectMeta{
				Name: "instance1-abc-def",
				Labels: map[string]string{
					naming.LabelRole:        naming.RolePostgresData,
					naming.LabelInstanceSet: "instance1",
					naming.LabelInstance:    "instance1-abc"}}},
			{ObjectMeta: metav1.ObjectMeta{
				Name: "instance1-abc-def",
				Labels: map[string]string{
					naming.LabelRole:        naming.RolePostgresWAL,
					naming.LabelInstanceSet: "instance1",
					naming.LabelInstance:    "instance1-abc"}}}},
		expectedInstanceNames: []string{},
	}, {
		set: v1beta1.PostgresInstanceSetSpec{Name: "instance1",
			WALVolumeClaimSpec: &corev1.PersistentVolumeClaimSpec{}},
		fakeObservedInstances: newObservedInstances(
			&v1beta1.PostgresCluster{Spec: v1beta1.PostgresClusterSpec{
				InstanceSets: []v1beta1.PostgresInstanceSetSpec{{Name: "instance1"}},
			}},
			[]appsv1.StatefulSet{},
			[]corev1.Pod{},
		),
		fakeClusterVolumes: []corev1.PersistentVolumeClaim{
			{ObjectMeta: metav1.ObjectMeta{
				Name: "instance1-def-ghi",
				Labels: map[string]string{
					naming.LabelRole:        naming.RolePostgresData,
					naming.LabelInstanceSet: "instance1",
					naming.LabelInstance:    "instance1-def"}}},
			{ObjectMeta: metav1.ObjectMeta{
				Name: "instance1-def-jkl",
				Labels: map[string]string{
					naming.LabelRole:        naming.RolePostgresWAL,
					naming.LabelInstanceSet: "instance1",
					naming.LabelInstance:    "instance1-def"}}}},
		expectedInstanceNames: []string{"instance1-def"},
	}, {
		set: v1beta1.PostgresInstanceSetSpec{Name: "instance1",
			WALVolumeClaimSpec: &corev1.PersistentVolumeClaimSpec{}},
		fakeObservedInstances: newObservedInstances(
			&v1beta1.PostgresCluster{Spec: v1beta1.PostgresClusterSpec{
				InstanceSets: []v1beta1.PostgresInstanceSetSpec{{Name: "instance1"}},
			}},
			[]appsv1.StatefulSet{},
			[]corev1.Pod{},
		),
		fakeClusterVolumes: []corev1.PersistentVolumeClaim{{ObjectMeta: metav1.ObjectMeta{
			Name: "instance1-def-ghi",
			Labels: map[string]string{
				naming.LabelRole:        naming.RolePostgresData,
				naming.LabelInstanceSet: "instance1",
				naming.LabelInstance:    "instance1-def"}}}},
		expectedInstanceNames: []string{},
	}}

	for _, tc := range testCases {
		var walEnabled string
		if tc.set.WALVolumeClaimSpec != nil {
			walEnabled = ", WAL volume enabled"
		}
		name := fmt.Sprintf("%d set(s), %d volume(s)%s: expect %d instance names(s)",
			len(tc.fakeObservedInstances.setNames), len(tc.fakeClusterVolumes), walEnabled,
			len(tc.expectedInstanceNames))
		t.Run(name, func(t *testing.T) {
			assert.DeepEqual(t, findAvailableInstanceNames(tc.set, tc.fakeObservedInstances,
				tc.fakeClusterVolumes), tc.expectedInstanceNames)
		})
	}
}

func TestReconcileUpgrade(t *testing.T) {
	// setup the test environment and ensure a clean teardown
	tEnv, tClient, cfg := setupTestEnv(t, ControllerName)
	t.Cleanup(func() { teardownTestEnv(t, tEnv) })
	r := &Reconciler{}
	ctx, cancel := setupManager(t, cfg, func(mgr manager.Manager) {
		r = &Reconciler{
			Client:   mgr.GetClient(),
			Recorder: mgr.GetEventRecorderFor(ControllerName),
			Tracer:   otel.Tracer(ControllerName),
			Owner:    ControllerName,
		}
	})
	t.Cleanup(func() { teardownManager(cancel, t) })

	ns := &corev1.Namespace{}
	ns.GenerateName = "postgres-operator-test-"
	ns.Labels = labels.Set{"postgres-operator-test": t.Name()}
	assert.NilError(t, tClient.Create(ctx, ns))
	t.Cleanup(func() { assert.Check(t, tClient.Delete(ctx, ns)) })

	sa := &corev1.ServiceAccount{
		ObjectMeta: metav1.ObjectMeta{Name: "hippo-sa"},
	}

	obs := &observedInstances{
		forCluster: []*Instance{{
			Name: "instance1",
			Pods: []*corev1.Pod{{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{naming.LabelRole: naming.RolePatroniLeader},
				},
				Status: corev1.PodStatus{
					ContainerStatuses: []corev1.ContainerStatus{{
						Name: "database",
						State: corev1.ContainerState{
							Running: &corev1.ContainerStateRunning{},
						},
					}},
				},
			}},
			Spec: &v1beta1.PostgresInstanceSetSpec{
				Name: "instance1",
			},
		}},
	}

	testCases := []struct {
		// a description of the test
		testDesc string
		// whether or not to mock cluster endpoints
		createEndpoints bool
		// conditions to apply to the mock postgrescluster
		clusterConditions map[string]metav1.ConditionStatus
		// the status to apply to the mock postgrescluster
		status *v1beta1.PostgresClusterStatus
		// the upgrade field to define in the postgrescluster spec for the test
		upgrade *v1beta1.PGMajorUpgrade
		// whether or not the test should expect a Job to be reconciled
		expectReconcile bool
		// expected return value
		expectedReturnEarly bool
	}{{
		testDesc: "upgrade not enabled",
		upgrade: &v1beta1.PGMajorUpgrade{
			Enabled:             initialize.Bool(false),
			FromPostgresVersion: 12,
			Image:               initialize.String("upgrade-image"),
		},
		status:              &v1beta1.PostgresClusterStatus{},
		expectReconcile:     false,
		expectedReturnEarly: false,
	}, {
		testDesc:        "upgrade enabled, no upgrade job, endpoints exist",
		createEndpoints: true,
		upgrade: &v1beta1.PGMajorUpgrade{
			Enabled:             initialize.Bool(true),
			FromPostgresVersion: 12,
			Image:               initialize.String("upgrade-image"),
		},
		status:              &v1beta1.PostgresClusterStatus{},
		expectReconcile:     false,
		expectedReturnEarly: true,
	}, {
		testDesc:        "upgrade enabled, no upgrade job, endpoints do not exist, completed condition not set",
		createEndpoints: false,
		upgrade: &v1beta1.PGMajorUpgrade{
			Enabled:             initialize.Bool(true),
			FromPostgresVersion: 12,
			Image:               initialize.String("upgrade-image"),
		},
		status: &v1beta1.PostgresClusterStatus{
			StartupInstance:    "instance1-abcd",
			StartupInstanceSet: "instance1",
		},
		expectReconcile:     true,
		expectedReturnEarly: true,
	}, {
		testDesc:        "upgrade enabled, created",
		createEndpoints: false,
		upgrade: &v1beta1.PGMajorUpgrade{
			Enabled:             initialize.Bool(true),
			FromPostgresVersion: 12,
			Image:               initialize.String("upgrade-image"),
		},
		status: &v1beta1.PostgresClusterStatus{
			StartupInstance:    "instance1-abcd",
			StartupInstanceSet: "instance1",
		},
		clusterConditions: map[string]metav1.ConditionStatus{
			ConditionPGUpgradeCompleted: metav1.ConditionTrue,
		},
		expectReconcile:     true,
		expectedReturnEarly: false,
	}}

	for i, tc := range testCases {
		clusterName := "pg-upgrade-" + strconv.Itoa(i)

		t.Run(tc.testDesc, func(t *testing.T) {

			ctx := context.Background()
			cluster := fakeUpgradeCluster(clusterName, ns.GetName(), "")
			cluster.Spec.Upgrade = tc.upgrade
			cluster.Status = *tc.status
			for condition, status := range tc.clusterConditions {
				meta.SetStatusCondition(&cluster.Status.Conditions, metav1.Condition{
					Type: condition, Reason: "testing", Status: status})
			}
			assert.NilError(t, tClient.Create(ctx, cluster))
			t.Cleanup(func() {
				// Remove finalizers, if any, so the namespace can terminate.
				assert.Check(t, client.IgnoreNotFound(
					tClient.Patch(ctx, cluster, client.RawPatch(
						client.Merge.Type(), []byte(`{"metadata":{"finalizers":[]}}`)))))
			})
			assert.NilError(t, tClient.Status().Update(ctx, cluster))

			if tc.createEndpoints {
				fakeLeaderEP := corev1.Endpoints{}
				fakeLeaderEP.ObjectMeta = naming.PatroniLeaderEndpoints(cluster)
				fakeLeaderEP.ObjectMeta.Namespace = ns.Name
				assert.NilError(t, r.Client.Create(ctx, &fakeLeaderEP))
				fakeDCSEP := corev1.Endpoints{}
				fakeDCSEP.ObjectMeta = naming.PatroniDistributedConfiguration(cluster)
				fakeDCSEP.ObjectMeta.Namespace = ns.Name
				assert.NilError(t, r.Client.Create(ctx, &fakeDCSEP))
				fakeFailoverEP := corev1.Endpoints{}
				fakeFailoverEP.ObjectMeta = naming.PatroniTrigger(cluster)
				fakeFailoverEP.ObjectMeta.Namespace = ns.Name
				assert.NilError(t, r.Client.Create(ctx, &fakeFailoverEP))
			}

			// resources needed for reconcileUpgradeJob
			spec := []v1beta1.PostgresInstanceSetSpec{{
				Name:                "instance1",
				Replicas:            Int32(1),
				DataVolumeClaimSpec: testVolumeClaimSpec(),
			}}
			clusterCerts := &corev1.SecretProjection{
				LocalObjectReference: corev1.LocalObjectReference{
					Name: "cluster-certs",
				},
			}
			clientCerts := &corev1.SecretProjection{
				LocalObjectReference: corev1.LocalObjectReference{
					Name: "client-certs",
				},
			}
			volumes := []corev1.PersistentVolumeClaim{{}}

			returnEarly, err := r.reconcileUpgradeJob(ctx, cluster, obs, spec, sa.Name, clusterCerts, clientCerts, volumes)
			assert.NilError(t, err)
			assert.Equal(t, returnEarly, tc.expectedReturnEarly)

			if tc.expectReconcile {
				// verify expected behavior when a reconcile is expected
				jobs := &batchv1.JobList{}
				err := tClient.List(ctx, jobs, &client.ListOptions{
					LabelSelector: naming.PGUpgradeJobSelector(clusterName),
				})
				assert.NilError(t, err)
				assert.Assert(t, len(jobs.Items) == 1)

				var foundOwnershipRef bool
				for _, r := range jobs.Items[0].GetOwnerReferences() {
					if r.Kind == "PostgresCluster" && r.Name == clusterName &&
						r.UID == cluster.GetUID() {
						foundOwnershipRef = true
						break
					}
				}
				assert.Assert(t, foundOwnershipRef)
				return
			} else {
				// verify expected results when a reconcile is not expected
				jobs := &batchv1.JobList{}
				// use a pgupgrade selector to check for the existence of any jobs
				err := tClient.List(ctx, jobs, &client.ListOptions{
					LabelSelector: naming.PGUpgradeJobSelector(clusterName),
				})
				assert.NilError(t, err)
				assert.Assert(t, len(jobs.Items) == 0)

				return
			}
		})
	}
}

func TestObserveUpgradeEnv(t *testing.T) {

	// setup the test environment and ensure a clean teardown
	tEnv, tClient, cfg := setupTestEnv(t, ControllerName)
	t.Cleanup(func() { teardownTestEnv(t, tEnv) })
	r := &Reconciler{}
	ctx, cancel := setupManager(t, cfg, func(mgr manager.Manager) {
		r = &Reconciler{
			Client:   tClient,
			Recorder: mgr.GetEventRecorderFor(ControllerName),
			Tracer:   otel.Tracer(ControllerName),
			Owner:    ControllerName,
		}
	})
	t.Cleanup(func() { teardownManager(cancel, t) })

	ns := &corev1.Namespace{}
	ns.GenerateName = "postgres-operator-test-"
	ns.Labels = labels.Set{"postgres-operator-test": t.Name()}
	assert.NilError(t, tClient.Create(ctx, ns))
	t.Cleanup(func() { assert.Check(t, tClient.Delete(ctx, ns)) })
	namespace := ns.Name

	generateJob := func(clusterName string, completed, failed *bool) *batchv1.Job {

		cluster := &v1beta1.PostgresCluster{
			ObjectMeta: metav1.ObjectMeta{
				Name:      clusterName,
				Namespace: namespace,
			},
		}
		meta := naming.PGUpgradeJob(cluster)
		labels := naming.PGUpgradeJobLabels(cluster.Name)
		meta.Labels = labels
		meta.Annotations = map[string]string{naming.PGBackRestConfigHash: "testhash"}

		upgradeJob := &batchv1.Job{
			ObjectMeta: meta,
			Spec: batchv1.JobSpec{
				Template: corev1.PodTemplateSpec{
					ObjectMeta: meta,
					Spec: corev1.PodSpec{
						Containers: []corev1.Container{{
							Image: "test",
							Name:  naming.ContainerPGUpgrade,
						}},
						RestartPolicy: corev1.RestartPolicyNever,
					},
				},
			},
		}

		if completed != nil {
			if *completed {
				upgradeJob.Status.Conditions = append(upgradeJob.Status.Conditions, batchv1.JobCondition{
					Type:    batchv1.JobComplete,
					Status:  corev1.ConditionTrue,
					Reason:  "test",
					Message: "test",
				})
			} else {
				upgradeJob.Status.Conditions = append(upgradeJob.Status.Conditions, batchv1.JobCondition{
					Type:    batchv1.JobComplete,
					Status:  corev1.ConditionFalse,
					Reason:  "test",
					Message: "test",
				})
			}
		} else if failed != nil {
			if *failed {
				upgradeJob.Status.Conditions = append(upgradeJob.Status.Conditions, batchv1.JobCondition{
					Type:    batchv1.JobFailed,
					Status:  corev1.ConditionTrue,
					Reason:  "test",
					Message: "test",
				})
			} else {
				upgradeJob.Status.Conditions = append(upgradeJob.Status.Conditions, batchv1.JobCondition{
					Type:    batchv1.JobFailed,
					Status:  corev1.ConditionFalse,
					Reason:  "test",
					Message: "test",
				})
			}
		}

		return upgradeJob
	}

	type testResult struct {
		foundUpgradeJob          bool
		endpointCount            int
		expectedClusterCondition *metav1.Condition
	}

	testCases := []struct {
		desc            string
		createResources func(t *testing.T, cluster *v1beta1.PostgresCluster)
		result          testResult
	}{{
		desc: "upgrade job and all patroni endpoints exist",
		createResources: func(t *testing.T, cluster *v1beta1.PostgresCluster) {
			fakeLeaderEP := &corev1.Endpoints{}
			fakeLeaderEP.ObjectMeta = naming.PatroniLeaderEndpoints(cluster)
			fakeLeaderEP.ObjectMeta.Namespace = namespace
			assert.NilError(t, r.Client.Create(ctx, fakeLeaderEP))
			fakeDCSEP := &corev1.Endpoints{}
			fakeDCSEP.ObjectMeta = naming.PatroniDistributedConfiguration(cluster)
			fakeDCSEP.ObjectMeta.Namespace = namespace
			assert.NilError(t, r.Client.Create(ctx, fakeDCSEP))
			fakeFailoverEP := &corev1.Endpoints{}
			fakeFailoverEP.ObjectMeta = naming.PatroniTrigger(cluster)
			fakeFailoverEP.ObjectMeta.Namespace = namespace
			assert.NilError(t, r.Client.Create(ctx, fakeFailoverEP))

			job := generateJob(cluster.Name, initialize.Bool(false), initialize.Bool(false))
			assert.NilError(t, r.Client.Create(ctx, job))
		},
		result: testResult{
			foundUpgradeJob:          true,
			endpointCount:            3,
			expectedClusterCondition: nil,
		},
	}, {
		desc: "patroni endpoints only exist",
		createResources: func(t *testing.T, cluster *v1beta1.PostgresCluster) {
			fakeLeaderEP := &corev1.Endpoints{}
			fakeLeaderEP.ObjectMeta = naming.PatroniLeaderEndpoints(cluster)
			fakeLeaderEP.ObjectMeta.Namespace = namespace
			assert.NilError(t, r.Client.Create(ctx, fakeLeaderEP))
			fakeDCSEP := &corev1.Endpoints{}
			fakeDCSEP.ObjectMeta = naming.PatroniDistributedConfiguration(cluster)
			fakeDCSEP.ObjectMeta.Namespace = namespace
			assert.NilError(t, r.Client.Create(ctx, fakeDCSEP))
			fakeFailoverEP := &corev1.Endpoints{}
			fakeFailoverEP.ObjectMeta = naming.PatroniTrigger(cluster)
			fakeFailoverEP.ObjectMeta.Namespace = namespace
			assert.NilError(t, r.Client.Create(ctx, fakeFailoverEP))
		},
		result: testResult{
			foundUpgradeJob:          false,
			endpointCount:            3,
			expectedClusterCondition: nil,
		},
	}, {
		desc: "upgrade job only exists",
		createResources: func(t *testing.T, cluster *v1beta1.PostgresCluster) {
			job := generateJob(cluster.Name, initialize.Bool(false), initialize.Bool(false))
			assert.NilError(t, r.Client.Create(ctx, job))
		},
		result: testResult{
			foundUpgradeJob:          true,
			endpointCount:            0,
			expectedClusterCondition: nil,
		},
	}, {
		desc: "upgrade job completed condition true",
		createResources: func(t *testing.T, cluster *v1beta1.PostgresCluster) {
			if strings.EqualFold(os.Getenv("USE_EXISTING_CLUSTER"), "true") {
				t.Skip("requires mocking of Job conditions")
			}
			job := generateJob(cluster.Name, initialize.Bool(true), nil)
			assert.NilError(t, r.Client.Create(ctx, job.DeepCopy()))
			assert.NilError(t, r.Client.Status().Update(ctx, job))
		},
		result: testResult{
			foundUpgradeJob: true,
			endpointCount:   0,
			expectedClusterCondition: &metav1.Condition{
				Type:    ConditionPGUpgradeCompleted,
				Status:  metav1.ConditionTrue,
				Reason:  "PGUpgradeComplete",
				Message: "pg_upgrade completed successfully",
			},
		},
	}, {
		desc: "upgrade job completed condition false",
		createResources: func(t *testing.T, cluster *v1beta1.PostgresCluster) {
			if strings.EqualFold(os.Getenv("USE_EXISTING_CLUSTER"), "true") {
				t.Skip("requires mocking of Job conditions")
			}
			job := generateJob(cluster.Name, nil, initialize.Bool(true))
			assert.NilError(t, r.Client.Create(ctx, job.DeepCopy()))
			assert.NilError(t, r.Client.Status().Update(ctx, job))
		},
		result: testResult{
			foundUpgradeJob: true,
			endpointCount:   0,
			expectedClusterCondition: &metav1.Condition{
				Type:    ConditionPGUpgradeCompleted,
				Status:  metav1.ConditionFalse,
				Reason:  "PGUpgradeFailed",
				Message: "pg_upgrade failed",
			},
		},
	}}

	for i, tc := range testCases {
		t.Run(tc.desc, func(t *testing.T) {

			clusterName := "observe-upgrade-env" + strconv.Itoa(i)
			clusterUID := clusterName
			cluster := fakeUpgradeCluster(clusterName, namespace, clusterUID)
			tc.createResources(t, cluster)

			endpoints, job, err := r.observeUpgradeEnv(ctx, cluster)
			assert.NilError(t, err)

			assert.Assert(t, tc.result.foundUpgradeJob == (job != nil))
			assert.Assert(t, tc.result.endpointCount == len(endpoints))

			if tc.result.expectedClusterCondition != nil {
				condition := meta.FindStatusCondition(cluster.Status.Conditions,
					tc.result.expectedClusterCondition.Type)
				if assert.Check(t, condition != nil) {
					assert.Equal(t, tc.result.expectedClusterCondition.Status, condition.Status)
					assert.Equal(t, tc.result.expectedClusterCondition.Reason, condition.Reason)
					assert.Equal(t, tc.result.expectedClusterCondition.Message, condition.Message)
				}
			}
		})
	}
}

func TestPrepareForUpgrade(t *testing.T) {

	// setup the test environment and ensure a clean teardown
	tEnv, tClient, cfg := setupTestEnv(t, ControllerName)
	t.Cleanup(func() { teardownTestEnv(t, tEnv) })
	r := &Reconciler{}
	ctx, cancel := setupManager(t, cfg, func(mgr manager.Manager) {
		r = &Reconciler{
			Client:   tClient,
			Recorder: mgr.GetEventRecorderFor(ControllerName),
			Tracer:   otel.Tracer(ControllerName),
			Owner:    ControllerName,
		}
	})
	t.Cleanup(func() { teardownManager(cancel, t) })

	ns := &corev1.Namespace{}
	ns.GenerateName = "postgres-operator-test-"
	ns.Labels = labels.Set{"postgres-operator-test": t.Name()}
	assert.NilError(t, tClient.Create(ctx, ns))
	t.Cleanup(func() { assert.Check(t, tClient.Delete(ctx, ns)) })
	namespace := ns.Name

	generateJob := func(clusterName string) *batchv1.Job {

		cluster := &v1beta1.PostgresCluster{
			ObjectMeta: metav1.ObjectMeta{
				Name:      clusterName,
				Namespace: namespace,
			},
		}
		meta := naming.PGUpgradeJob(cluster)
		labels := naming.PGUpgradeJobLabels(cluster.Name)
		meta.Labels = labels

		upgradeJob := &batchv1.Job{
			ObjectMeta: meta,
			Spec: batchv1.JobSpec{
				Template: corev1.PodTemplateSpec{
					ObjectMeta: meta,
					Spec: corev1.PodSpec{
						Containers: []corev1.Container{{
							Image: "test",
							Name:  naming.ContainerPGUpgrade,
						}},
						RestartPolicy: corev1.RestartPolicyNever,
					},
				},
			},
		}

		return upgradeJob
	}

	type testResult struct {
		upgradeJobExists         bool
		endpointCountZero        bool
		expectedClusterCondition *metav1.Condition
	}
	const primaryInstanceName = "primary-instance"
	const primaryInstanceSetName = "primary-instance-set"

	testCases := []struct {
		desc            string
		createResources func(t *testing.T, cluster *v1beta1.PostgresCluster) (*batchv1.Job, []corev1.Endpoints)
		upgradeEnabled  bool
		result          testResult
	}{{
		desc: "remove upgrade jobs",
		createResources: func(t *testing.T,
			cluster *v1beta1.PostgresCluster) (*batchv1.Job, []corev1.Endpoints) {
			job := generateJob(cluster.Name)
			assert.NilError(t, r.Client.Create(ctx, job))
			return job, nil
		},
		result: testResult{
			upgradeJobExists:  true,
			endpointCountZero: true,
			expectedClusterCondition: &metav1.Condition{
				Type:    ConditionPGUpgradeProgressing,
				Status:  metav1.ConditionTrue,
				Reason:  "PGUpgradeRequested",
				Message: "Preparing cluster for upgrade: removing existing upgrade job",
			},
		},
	}, {
		desc: "cluster fully prepared, primary as startup instance",
		createResources: func(t *testing.T,
			cluster *v1beta1.PostgresCluster) (*batchv1.Job, []corev1.Endpoints) {
			return nil, []corev1.Endpoints{}
		},
		result: testResult{
			upgradeJobExists:  false,
			endpointCountZero: true,
			expectedClusterCondition: &metav1.Condition{
				Type:    ConditionPGUpgradeProgressing,
				Status:  metav1.ConditionTrue,
				Reason:  ReasonReadyForUpgrade,
				Message: "Upgrading cluster postgres major version",
			},
		},
	}}

	for i, tc := range testCases {
		name := tc.desc
		t.Run(name, func(t *testing.T) {

			clusterName := "prepare-for-upgrade-" + strconv.Itoa(i)
			clusterUID := clusterName
			cluster := fakeUpgradeCluster(clusterName, namespace, clusterUID)
			if tc.upgradeEnabled {
				cluster.Spec.Upgrade = &v1beta1.PGMajorUpgrade{
					Enabled:             initialize.Bool(true),
					FromPostgresVersion: 12,
					Image:               initialize.String("test-upgrade-image"),
				}
			}
			cluster.Status.Patroni.SystemIdentifier = "abcde12345"
			cluster.Status.Proxy.PGBouncer.PostgreSQLRevision = "abcde12345"
			cluster.Status.Monitoring.ExporterConfiguration = "abcde12345"
			meta.SetStatusCondition(&cluster.Status.Conditions, metav1.Condition{
				ObservedGeneration: cluster.GetGeneration(),
				Type:               ConditionPostgresDataInitialized,
				Status:             metav1.ConditionTrue,
				Reason:             "PGUpgradeComplete",
				Message:            "pg_upgrade completed successfully",
			})

			job, endpoints := tc.createResources(t, cluster)

			upgradeJobs := &batchv1.JobList{}
			assert.NilError(t, r.Client.List(ctx, upgradeJobs, &client.ListOptions{
				LabelSelector: naming.PGUpgradeJobSelector(cluster.GetName()),
			}))
			assert.Assert(t, tc.result.upgradeJobExists == (len(upgradeJobs.Items) == 1))

			fakeObserved := &observedInstances{forCluster: []*Instance{{
				Name: primaryInstanceName,
				Spec: &v1beta1.PostgresInstanceSetSpec{Name: primaryInstanceSetName},
				Pods: []*corev1.Pod{{
					ObjectMeta: metav1.ObjectMeta{
						Labels: map[string]string{naming.LabelRole: naming.RolePatroniLeader},
					},
				}}},
			}}
			assert.NilError(t, r.prepareForUpgrade(ctx, cluster, fakeObserved,
				endpoints, job))

			var primaryInstance *Instance
			for i, instance := range fakeObserved.forCluster {
				isPrimary, _ := instance.IsPrimary()
				if isPrimary {
					primaryInstance = fakeObserved.forCluster[i]
				}
			}

			if primaryInstance != nil {
				assert.Assert(t, cluster.Status.StartupInstance == primaryInstanceName)
			} else {
				assert.Equal(t, cluster.Status.StartupInstance,
					naming.GenerateStartupInstance(cluster, &cluster.Spec.InstanceSets[0]).Name)
			}

			leaderEP, dcsEP, failoverEP := corev1.Endpoints{}, corev1.Endpoints{}, corev1.Endpoints{}
			currentEndpoints := []corev1.Endpoints{}
			if err := r.Client.Get(ctx, naming.AsObjectKey(naming.PatroniLeaderEndpoints(cluster)),
				&leaderEP); err != nil {
				assert.NilError(t, client.IgnoreNotFound(err))
			} else {
				currentEndpoints = append(currentEndpoints, leaderEP)
			}
			if err := r.Client.Get(ctx, naming.AsObjectKey(naming.PatroniDistributedConfiguration(cluster)),
				&dcsEP); err != nil {
				assert.NilError(t, client.IgnoreNotFound(err))
			} else {
				currentEndpoints = append(currentEndpoints, dcsEP)
			}
			if err := r.Client.Get(ctx, naming.AsObjectKey(naming.PatroniTrigger(cluster)),
				&failoverEP); err != nil {
				assert.NilError(t, client.IgnoreNotFound(err))
			} else {
				currentEndpoints = append(currentEndpoints, failoverEP)
			}

			if tc.result.endpointCountZero {
				assert.Assert(t, len(currentEndpoints) == 0)
			} else {
				assert.Assert(t, len(currentEndpoints) != 0)
			}

			if tc.result.expectedClusterCondition != nil {
				condition := meta.FindStatusCondition(cluster.Status.Conditions,
					tc.result.expectedClusterCondition.Type)
				if assert.Check(t, condition != nil) {
					assert.Equal(t, tc.result.expectedClusterCondition.Status, condition.Status)
					assert.Equal(t, tc.result.expectedClusterCondition.Reason, condition.Reason)
					assert.Equal(t, tc.result.expectedClusterCondition.Message, condition.Message)
				}
				if tc.result.expectedClusterCondition.Reason == ReasonReadyForUpgrade {
					assert.Assert(t, cluster.Status.Patroni.SystemIdentifier == "")
					assert.Assert(t, cluster.Status.Proxy.PGBouncer.PostgreSQLRevision == "")
					assert.Assert(t, cluster.Status.Monitoring.ExporterConfiguration == "")
					assert.Assert(t, meta.FindStatusCondition(cluster.Status.Conditions,
						ConditionPostgresDataInitialized) == nil)
				}
			}
		})
	}
}

func fakeUpgradeCluster(clusterName, namespace, clusterUID string) *v1beta1.PostgresCluster {
	cluster := &v1beta1.PostgresCluster{
		ObjectMeta: metav1.ObjectMeta{
			Name:      clusterName,
			Namespace: namespace,
			UID:       types.UID(clusterUID),
		},
		Spec: v1beta1.PostgresClusterSpec{
			Port:            initialize.Int32(5432),
			Shutdown:        initialize.Bool(false),
			PostgresVersion: 13,
			ImagePullSecrets: []corev1.LocalObjectReference{{
				Name: "myImagePullSecret"},
			},
			Image: "example.com/crunchy-postgres-ha:test",
			InstanceSets: []v1beta1.PostgresInstanceSetSpec{{
				Name: "instance1",
				DataVolumeClaimSpec: corev1.PersistentVolumeClaimSpec{
					AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteMany},
					Resources: corev1.ResourceRequirements{
						Requests: map[corev1.ResourceName]resource.Quantity{
							corev1.ResourceStorage: resource.MustParse("1Gi"),
						},
					},
				},
			}},
			Backups: v1beta1.Backups{
				PGBackRest: v1beta1.PGBackRestArchive{
					Repos: []v1beta1.PGBackRestRepo{{
						Name: "repo1",
						Volume: &v1beta1.RepoPVC{
							VolumeClaimSpec: corev1.PersistentVolumeClaimSpec{
								AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteMany},
								Resources: corev1.ResourceRequirements{
									Requests: map[corev1.ResourceName]resource.Quantity{
										corev1.ResourceStorage: resource.MustParse("1Gi"),
									},
								},
							},
						},
					}},
				},
			},
		},
	}

	return cluster
}
