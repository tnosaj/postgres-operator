package scheduler

/*
 Copyright 2019 - 2020 Crunchy Data Solutions, Inc.
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

import (
	"fmt"
	"time"

	"github.com/crunchydata/postgres-operator/internal/config"
	"github.com/crunchydata/postgres-operator/kubeapi"
	crv1 "github.com/crunchydata/postgres-operator/pkg/apis/crunchydata.com/v1"
	log "github.com/sirupsen/logrus"
	kerrors "k8s.io/apimachinery/pkg/api/errors"
)

type BackRestBackupJob struct {
	backupType  string
	stanza      string
	namespace   string
	deployment  string
	label       string
	container   string
	cluster     string
	storageType string
	options     string
}

func (s *ScheduleTemplate) NewBackRestSchedule() BackRestBackupJob {
	return BackRestBackupJob{
		backupType:  s.PGBackRest.Type,
		stanza:      "db",
		namespace:   s.Namespace,
		deployment:  s.PGBackRest.Deployment,
		label:       s.PGBackRest.Label,
		container:   s.PGBackRest.Container,
		cluster:     s.Cluster,
		storageType: s.PGBackRest.StorageType,
		options:     s.Options,
	}
}

func (b BackRestBackupJob) Run() {
	contextLogger := log.WithFields(log.Fields{
		"namespace":   b.namespace,
		"deployment":  b.deployment,
		"label":       b.label,
		"container":   b.container,
		"backupType":  b.backupType,
		"cluster":     b.cluster,
		"storageType": b.storageType})

	contextLogger.Info("Running pgBackRest backup")

	cluster := crv1.Pgcluster{}
	found, err := kubeapi.Getpgcluster(restClient, &cluster, b.cluster, b.namespace)

	if !found {
		contextLogger.WithFields(log.Fields{
			"error": err,
		}).Error("pgCluster not found")
		return
	} else if err != nil {
		contextLogger.WithFields(log.Fields{
			"error": err,
		}).Error("error retrieving pgCluster")
		return
	}

	taskName := fmt.Sprintf("%s-%s-sch-backup", b.cluster, b.backupType)

	//if the cluster is found, check for an annotation indicating it has not been upgraded
	//if the annotation does not exist, then it is a new cluster and proceed as usual
	//if the annotation is set to "true", the cluster has already been upgraded and can proceed but
	//if the annotation is set to "false", this cluster will need to be upgraded before proceeding
	//log the issue, then return
	if cluster.Annotations[config.ANNOTATION_IS_UPGRADED] == config.ANNOTATIONS_FALSE {
		contextLogger.WithFields(log.Fields{
			"task": taskName,
		}).Debug("pgcluster requires an upgrade before scheduled pgbackrest task can be run")
		return
	}

	result := crv1.Pgtask{}
	found, err = kubeapi.Getpgtask(restClient, &result, taskName, b.namespace)

	if found {
		err := kubeapi.Deletepgtask(restClient, taskName, b.namespace)
		if err != nil {
			contextLogger.WithFields(log.Fields{
				"task":  taskName,
				"error": err,
			}).Error("error deleting pgTask")
			return
		}

		job, found := kubeapi.GetJob(kubeClient, taskName, b.namespace)

		if found {
			err = kubeapi.DeleteJob(kubeClient, taskName, b.namespace)
			if err != nil {
				contextLogger.WithFields(log.Fields{
					"task":  taskName,
					"error": err,
				}).Error("error deleting backup job")
				return
			}

			timeout := time.Second * 60
			err = kubeapi.IsJobDeleted(kubeClient, b.namespace, job, timeout)
			if err != nil {
				contextLogger.WithFields(log.Fields{
					"task":  taskName,
					"error": err,
				}).Error("error waiting for job to delete")
				return
			}
		}
	} else if err != nil && !kerrors.IsNotFound(err) {
		contextLogger.WithFields(log.Fields{
			"task":  taskName,
			"error": err,
		}).Error("error getting pgTask")
		return
	}

	selector := fmt.Sprintf("%s=%s,pgo-backrest-repo=true", config.LABEL_PG_CLUSTER, b.cluster)
	pods, err := kubeapi.GetPods(kubeClient, selector, b.namespace)
	if err != nil {
		contextLogger.WithFields(log.Fields{
			"selector": selector,
			"error":    err,
		}).Error("error getting pods from selector")
		return
	}

	if len(pods.Items) != 1 {
		contextLogger.WithFields(log.Fields{
			"selector":  selector,
			"error":     err,
			"podsFound": len(pods.Items),
		}).Error("pods returned does not equal 1, it should")
		return
	}

	backrest := pgBackRestTask{
		clusterName:   cluster.Name,
		taskName:      taskName,
		podName:       pods.Items[0].Name,
		containerName: "database",
		backupOptions: fmt.Sprintf("--type=%s %s", b.backupType, b.options),
		stanza:        b.stanza,
		storageType:   b.storageType,
		imagePrefix:   cluster.Spec.PGOImagePrefix,
	}

	err = kubeapi.Createpgtask(restClient, backrest.NewBackRestTask(), b.namespace)
	if err != nil {
		contextLogger.WithFields(log.Fields{
			"error": err,
		}).Error("could not create new pgtask")
		return
	}
}
