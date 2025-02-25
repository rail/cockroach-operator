/*
Copyright 2021 The Cockroach Authors

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    https://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package testutil

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/csv"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"k8s.io/client-go/kubernetes"

	api "github.com/cockroachdb/cockroach-operator/apis/v1alpha1"
	"github.com/cockroachdb/cockroach-operator/pkg/database"
	"github.com/cockroachdb/cockroach-operator/pkg/kube"
	"github.com/cockroachdb/cockroach-operator/pkg/labels"
	"github.com/cockroachdb/cockroach-operator/pkg/resource"
	testenv "github.com/cockroachdb/cockroach-operator/pkg/testutil/env"
	"github.com/google/go-cmp/cmp"
	"github.com/pkg/errors"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apiresource "k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
)

// RequireClusterToBeReadyEventuallyTimeout tests to see if a statefulset has started correctly and
// all of the pods are running.
func RequireClusterToBeReadyEventuallyTimeout(t *testing.T, sb testenv.DiffingSandbox, b ClusterBuilder, timeout time.Duration) {
	cluster := b.Cluster()

	err := wait.Poll(10*time.Second, timeout, func() (bool, error) {

		ss, err := fetchStatefulSet(sb, cluster.StatefulSetName())
		if err != nil {
			t.Logf("error fetching stateful set")
			return false, err
		}

		if ss == nil {
			t.Logf("stateful set is not found")
			return false, nil
		}

		if !statefulSetIsReady(ss) {
			t.Logf("stateful set is not ready")
			logPods(context.TODO(), ss, cluster, sb, t)
			return false, nil
		}
		return true, nil
	})
	require.NoError(t, err)
}

// TODO are we using this??

func RequireClusterToBeReadyEventually(t *testing.T, sb testenv.DiffingSandbox, b ClusterBuilder) {
	cluster := b.Cluster()

	err := wait.Poll(10*time.Second, 60*time.Second, func() (bool, error) {

		ss, err := fetchStatefulSet(sb, cluster.StatefulSetName())
		if err != nil {
			t.Logf("error fetching stateful set")
			return false, err
		}

		if ss == nil {
			t.Logf("stateful set is not found")
			return false, nil
		}
		if !statefulSetIsReady(ss) {
			t.Logf("stateful set is not ready")
			return false, nil
		}
		return true, nil
	})
	require.NoError(t, err)
}

// RequireDbContainersToUseImage checks that the database is using the correct image
func RequireDbContainersToUseImage(t *testing.T, sb testenv.DiffingSandbox, cr *api.CrdbCluster) {
	err := wait.Poll(10*time.Second, 400*time.Second, func() (bool, error) {
		pods, err := fetchPodsInStatefulSet(sb, labels.Common(cr).Selector(cr.Spec.AdditionalLabels))
		if err != nil {
			return false, err
		}

		if len(pods) < int(cr.Spec.Nodes) {
			return false, nil
		}

		res := testPodsWithPredicate(pods, func(p *corev1.Pod) bool {
			c, err := kube.FindContainer(resource.DbContainerName, &p.Spec)
			if err != nil {
				return false
			}
			if cr.Spec.Image.Name == "" {
				version := strings.ReplaceAll(cr.Spec.CockroachDBVersion, ".", "_")
				image := os.Getenv(fmt.Sprintf("RELATED_IMAGE_COCKROACH_%s", version))
				return c.Image == image
			}
			return c.Image == cr.Spec.Image.Name
		})

		return res, nil
	})

	require.NoError(t, err)
}

func clusterIsInitialized(t *testing.T, sb testenv.DiffingSandbox, name string) (bool, error) {
	expectedConditions := []api.ClusterCondition{
		{
			Type:   api.InitializedCondition,
			Status: metav1.ConditionFalse,
		},
	}

	actual := resource.ClusterPlaceholder(name)
	if err := sb.Get(actual); err != nil {
		t.Logf("failed to fetch current cluster status :(")
		return false, err
	}

	actualConditions := actual.Status.DeepCopy().Conditions

	// Reset condition time as it is not significant for the assertion
	var emptyTime metav1.Time
	for i := range actualConditions {
		actualConditions[i].LastTransitionTime = emptyTime
	}

	if !cmp.Equal(expectedConditions, actualConditions) {
		return false, nil
	}

	return true, nil
}

func clusterIsDecommissioned(t *testing.T, sb testenv.DiffingSandbox, name string) (bool, error) {
	expectedConditions := []api.ClusterCondition{
		{
			Type:   api.DecommissionCondition,
			Status: metav1.ConditionTrue,
		},
	}

	actual := resource.ClusterPlaceholder(name)
	if err := sb.Get(actual); err != nil {
		t.Logf("failed to fetch current cluster status :(")
		return false, err
	}

	actualConditions := actual.Status.DeepCopy().Conditions

	// Reset condition time as it is not significant for the assertion
	var emptyTime metav1.Time
	for i := range actualConditions {
		actualConditions[i].LastTransitionTime = emptyTime
	}
	if !cmp.Equal(expectedConditions, actualConditions) {
		return false, nil
	}

	return true, nil
}

func fetchStatefulSet(sb testenv.DiffingSandbox, name string) (*appsv1.StatefulSet, error) {
	ss := &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{
			Name: name,
		},
	}

	if err := sb.Get(ss); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, nil
		}

		return nil, err
	}

	return ss, nil
}

func fetchPodsInStatefulSet(sb testenv.DiffingSandbox, labels map[string]string) ([]corev1.Pod, error) {
	var pods corev1.PodList

	if err := sb.List(&pods, labels); err != nil {
		return nil, err
	}

	return pods.Items, nil
}

func testPodsWithPredicate(pods []corev1.Pod, pred func(*corev1.Pod) bool) bool {
	for i := range pods {
		if !pred(&pods[i]) {
			return false
		}
	}

	return true
}

func statefulSetIsReady(ss *appsv1.StatefulSet) bool {
	return ss.Status.ReadyReplicas == ss.Status.Replicas
}

// TODO we are not using this

func RequireDownGradeOptionSet(t *testing.T, sb testenv.DiffingSandbox, b ClusterBuilder, version string) {
	sb.Mgr.GetConfig()
	podName := fmt.Sprintf("%s-0.%s", b.Cluster().Name(), b.Cluster().Name())
	conn := &database.DBConnection{
		Ctx:    context.TODO(),
		Client: sb.Mgr.GetClient(),
		Port:   b.Cluster().Spec().SQLPort,
		UseSSL: true,

		RestConfig:   sb.Mgr.GetConfig(),
		ServiceName:  podName,
		Namespace:    sb.Namespace,
		DatabaseName: "system",

		RunningInsideK8s:            false,
		ClientCertificateSecretName: b.Cluster().ClientTLSSecretName(),
		RootCertificateSecretName:   b.Cluster().NodeTLSSecretName(),
	}

	// Create a new database connection for the update.
	db, err := database.NewDbConnection(conn)
	require.NoError(t, err)
	defer db.Close()

	r := db.QueryRowContext(context.TODO(), "SHOW CLUSTER SETTING cluster.preserve_downgrade_option")
	var value string
	if err := r.Scan(&value); err != nil {
		t.Fatal(err)
	}

	if value == "" {
		t.Errorf("downgrade_option is empty and should be set to %s", version)
	}

	if value != value {
		t.Errorf("downgrade_option is not set to %s, but is set to %s", version, value)
	}

}

// TODO I do not think this is correct.  Keith mentioned we need to check something else.

// RequireDecommisionNode requires that proper nodes are decommisioned
func RequireDecommissionNode(t *testing.T, sb testenv.DiffingSandbox, b ClusterBuilder, numNodes int32) {
	cluster := b.Cluster()

	err := wait.Poll(10*time.Second, 700*time.Second, func() (bool, error) {
		sts, err := fetchStatefulSet(sb, cluster.StatefulSetName())
		if err != nil {
			t.Logf("statefulset is not found %v", err)
			return false, err
		}

		if sts == nil {
			t.Log("statefulset is not found")
			return false, nil
		}

		if !statefulSetIsReady(sts) {
			t.Log("statefulset is not ready")
			return false, nil
		}

		if numNodes != sts.Status.Replicas {
			t.Log("statefulset replicas do not match")
			return false, nil
		}
		//
		err = makeDrainStatusChecker(t, sb, b, uint64(numNodes))
		if err != nil {
			t.Logf("makeDrainStatusChecker failed due to error %v\n", err)
			return false, nil
		}
		return true, nil
	})
	require.NoError(t, err)
}

func makeDrainStatusChecker(t *testing.T, sb testenv.DiffingSandbox, b ClusterBuilder, numNodes uint64) error {
	cluster := b.Cluster()
	cmd := []string{"/cockroach/cockroach", "node", "status", "--decommission", "--format=csv", cluster.SecureMode()}
	podname := fmt.Sprintf("%s-0", cluster.StatefulSetName())
	stdout, stderror, err := kube.ExecInPod(sb.Mgr.GetScheme(), sb.Mgr.GetConfig(), sb.Namespace,
		podname, resource.DbContainerName, cmd)
	if err != nil || stderror != "" {
		t.Logf("exec cmd = %s on pod=%s exit with error %v and stdError %s and ns %s", cmd, podname, err, stderror, sb.Namespace)
		return err
	}
	r := csv.NewReader(strings.NewReader(stdout))
	// skip header
	if _, err := r.Read(); err != nil {
		return err
	}
	// We are using the host to filter the decommissioned node.
	// Currently the id does not match the pod index because of the
	// pod parallel strategy
	host := fmt.Sprintf("%s-%d.%s.%s", cluster.StatefulSetName(),
		numNodes, cluster.StatefulSetName(), sb.Namespace)
	for {
		record, err := r.Read()
		if err == io.EOF {
			break
		}

		if err != nil {
			return errors.Wrapf(err, "failed to get node draining status")
		}

		idStr, address := record[0], record[1]

		if !strings.Contains(address, host) {
			continue
		}
		//if the address is for the last pod that was decommissioned we are checking the replicas
		id, err := strconv.ParseUint(idStr, 10, 32)
		if err != nil {
			return errors.Wrap(err, "failed to extract node id from string")
		}

		isLive, replicasStr, isDecommissioning := record[8], record[9], record[10]
		t.Logf("draining node do to decommission test\n")
		t.Logf("id=%s\n ", idStr)
		t.Logf("address=%s\n ", address)
		t.Logf("isLive=%s\n ", isLive)
		t.Logf("replicas=%s\n", replicasStr)
		t.Logf("isDecommissioning=%v\n", isDecommissioning)

		// we are not checking isLive != "true"  on tests because the operator exits with islive=true
		// and when the checks for the test run the node is already decommissioned so isLive can be false
		if isDecommissioning != "true" {
			return errors.New("unexpected node status")
		}

		replicas, err := strconv.ParseUint(replicasStr, 10, 64)
		if err != nil {
			return errors.Wrap(err, "failed to parse replicas number")
		}
		// Node has finished draining successfully if replicas=0
		// otherwise we will signal an error, so the backoff logic retry until replicas=0 or timeout
		if replicas != 0 {
			return errors.Wrap(err, fmt.Sprintf("node %d has not completed draining yet", id))
		}
	}

	return nil
}

// RequireDatabaseToFunctionInsecure tests that the database is functioning correctly on an
// db that is insecure.
func RequireDatabaseToFunctionInsecure(t *testing.T, sb testenv.DiffingSandbox, b ClusterBuilder) {
	requireDatabaseToFunction(t, sb, b, false)
}

// RequireDatabaseToFunction tests that the database is functioning correctly
// for a db cluster that is using an SSL certificate.
func RequireDatabaseToFunction(t *testing.T, sb testenv.DiffingSandbox, b ClusterBuilder) {
	requireDatabaseToFunction(t, sb, b, true)
}

func requireDatabaseToFunction(t *testing.T, sb testenv.DiffingSandbox, b ClusterBuilder, useSSL bool) {
	sb.Mgr.GetConfig()
	podName := fmt.Sprintf("%s-0.%s", b.Cluster().Name(), b.Cluster().Name())

	conn := &database.DBConnection{
		Ctx:    context.TODO(),
		Client: sb.Mgr.GetClient(),
		Port:   b.Cluster().Spec().SQLPort,
		UseSSL: useSSL,

		RestConfig:   sb.Mgr.GetConfig(),
		ServiceName:  podName,
		Namespace:    sb.Namespace,
		DatabaseName: "system",

		RunningInsideK8s: false,
	}

	// set the client certs since we are using SSL
	if useSSL {
		conn.ClientCertificateSecretName = b.Cluster().ClientTLSSecretName()
		conn.RootCertificateSecretName = b.Cluster().NodeTLSSecretName()
	}

	// Create a new database connection for the update.
	db, err := database.NewDbConnection(conn)
	require.NoError(t, err)
	defer db.Close()

	if _, err := db.Exec("CREATE DATABASE test_db"); err != nil {
		t.Fatal(err)
	}

	if _, err := db.Exec("USE test_db"); err != nil {
		t.Fatal(err)
	}

	// Create the "accounts" table.
	if _, err := db.Exec("CREATE TABLE IF NOT EXISTS accounts (id INT PRIMARY KEY, balance INT)"); err != nil {
		t.Fatal(err)
	}

	// Insert two rows into the "accounts" table.
	if _, err := db.Exec(
		"INSERT INTO accounts (id, balance) VALUES (1, 1000), (2, 250)"); err != nil {
		t.Fatal(err)
	}

	// Print out the balances.
	rows, err := db.Query("SELECT id, balance FROM accounts")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	t.Log("Initial balances:")
	for rows.Next() {
		var id, balance int
		if err := rows.Scan(&id, &balance); err != nil {
			t.Fatal(err)
		}
		t.Log("balances", id, balance)
	}

	countRows, err := db.Query("SELECT COUNT(*) as count FROM accounts")
	if err != nil {
		t.Fatal(err)
	}
	defer countRows.Close()
	count := getCount(t, countRows)
	if count != 2 {
		t.Fatal(fmt.Errorf("found incorrect number of rows.  Expected 2 got %v", count))
	}

	t.Log("finished testing database")
}

func getCount(t *testing.T, rows *sql.Rows) (count int) {
	for rows.Next() {
		err := rows.Scan(&count)
		if err != nil {
			t.Fatal(err)
		}
	}
	return count
}

// RequirePVCToResize checks that the PVCs are resized correctly
func RequirePVCToResize(t *testing.T, sb testenv.DiffingSandbox, b ClusterBuilder, quantity apiresource.Quantity) {
	cluster := b.Cluster()

	// TODO rewrite this
	err := wait.Poll(10*time.Second, 500*time.Second, func() (bool, error) {
		ss, err := fetchStatefulSet(sb, cluster.StatefulSetName())
		if err != nil {
			return false, err
		}

		if ss == nil {
			t.Logf("stateful set is not found")
			return false, nil
		}

		if !statefulSetIsReady(ss) {
			return false, nil
		}
		clientset, err := kubernetes.NewForConfig(sb.Mgr.GetConfig())
		require.NoError(t, err)

		resized, err := resizedPVCs(context.TODO(), ss, b.Cluster(), clientset, t, quantity)
		require.NoError(t, err)

		return resized, nil
	})
	require.NoError(t, err)
}

// test to see if all PVCs are resized
func resizedPVCs(ctx context.Context, sts *appsv1.StatefulSet, cluster *resource.Cluster,
	clientset *kubernetes.Clientset, t *testing.T, quantity apiresource.Quantity) (bool, error) {

	prefixes := make([]string, len(sts.Spec.VolumeClaimTemplates))
	pvcsToKeep := make(map[string]bool, int(*sts.Spec.Replicas)*len(sts.Spec.VolumeClaimTemplates))
	for j, pvct := range sts.Spec.VolumeClaimTemplates {
		prefixes[j] = fmt.Sprintf("%s-%s-", pvct.Name, sts.Name)

		for i := int32(0); i < *sts.Spec.Replicas; i++ {
			name := fmt.Sprintf("%s-%s-%d", pvct.Name, sts.Name, i)
			pvcsToKeep[name] = true
		}
	}

	selector, err := metav1.LabelSelectorAsSelector(sts.Spec.Selector)
	if err != nil {
		return false, err
	}

	pvcs, err := clientset.CoreV1().PersistentVolumeClaims(cluster.Namespace()).List(ctx, metav1.ListOptions{
		LabelSelector: selector.String(),
	})

	if err != nil {
		return false, err
	}

	for _, pvc := range pvcs.Items {
		t.Logf("checking pvc %s", pvc.Name)
		// Resize PVCs that are still in use
		if pvcsToKeep[pvc.Name] {
			if !pvc.Spec.Resources.Requests.Storage().Equal(quantity) {
				return false, nil
			}
		}
	}

	return true, nil
}

func logPods(ctx context.Context, sts *appsv1.StatefulSet, cluster *resource.Cluster,
	sb testenv.DiffingSandbox, t *testing.T) error {
	// create a new clientset to talk to k8s
	clientset, err := kubernetes.NewForConfig(sb.Mgr.GetConfig())
	if err != nil {
		return err
	}

	// the LableSelector I thought worked did not
	// so I just get all of the Pods in a NS
	options := metav1.ListOptions{
		//LabelSelector: "app=" + cluster.StatefulSetName(),
	}

	// Get all pods
	podList, err := clientset.CoreV1().Pods(sts.Namespace).List(ctx, options)
	if err != nil {
		return err
	}

	if len(podList.Items) == 0 {
		t.Log("no pods found")
	}

	// Print out pretty into on the Pods
	for _, podInfo := range (*podList).Items {
		t.Logf("pods-name=%v\n", podInfo.Name)
		t.Logf("pods-status=%v\n", podInfo.Status.Phase)
		t.Logf("pods-condition=%v\n", podInfo.Status.Conditions)
		/*
			// TODO if pod is running but not ready for some period get pod logs
			if kube.IsPodReady(&podInfo) {
				t.Logf("pods-condition=%v\n", podInfo.Status.Conditions)
			}
		*/
	}

	return nil
}

func getPodLog(ctx context.Context, podName string, namespace string, clientset kubernetes.Interface) (string, error) {

	// This func will print out the pod logs
	// This is code that is used by version checker and we should probably refactor
	// this and move it into kube package.
	// But right now it is untested
	podLogOpts := corev1.PodLogOptions{}
	req := clientset.CoreV1().Pods(namespace).GetLogs(podName, &podLogOpts)

	podLogs, err := req.Stream(ctx)
	if err != nil {
		msg := "error in opening stream"
		return "", errors.Wrapf(err, msg)
	}
	defer podLogs.Close()

	buf := new(bytes.Buffer)
	_, err = io.Copy(buf, podLogs)
	if err != nil {
		msg := "error in copying stream"
		return "", errors.Wrapf(err, msg)
	}
	return buf.String(), nil
}

// RequirePVCToResize checks that the PVCs are resized correctly
func RequireNumberOfPVCs(t *testing.T, ctx context.Context, sb testenv.DiffingSandbox, b ClusterBuilder, quantity int) {
	cluster := b.Cluster()
	var boundPVCCount = 0

	// TODO rewrite this
	err := wait.Poll(10*time.Second, 500*time.Second, func() (bool, error) {
		clientset, err := kubernetes.NewForConfig(sb.Mgr.GetConfig())
		require.NoError(t, err)

		sts, err := fetchStatefulSet(sb, cluster.StatefulSetName())

		selector, err := metav1.LabelSelectorAsSelector(sts.Spec.Selector)
		if err != nil {
			return false, err
		}

		pvcs, err := clientset.CoreV1().PersistentVolumeClaims(cluster.Namespace()).List(ctx, metav1.ListOptions{
			LabelSelector: selector.String(),
		})

		if err != nil {
			return false, err
		}

		for _, pvc := range pvcs.Items {
			if pvc.Status.Phase == corev1.ClaimBound {
				boundPVCCount = boundPVCCount + 1
			}
		}

		return true, nil
	})
	require.NoError(t, err)

	require.Equal(t, quantity, boundPVCCount)
}
