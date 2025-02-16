package v2beta1

import (
	"context"
	"encoding/json"
	"fmt"

	semver "github.com/Masterminds/semver/v3"
	appsv2beta1 "github.com/emqx/emqx-operator/apis/apps/v2beta1"
	innerReq "github.com/emqx/emqx-operator/internal/requester"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

type updatePodConditions struct {
	*EMQXReconciler
}

func (u *updatePodConditions) reconcile(ctx context.Context, instance *appsv2beta1.EMQX, r innerReq.RequesterInterface) subResult {
	updateRs, _, _ := getReplicaSetList(ctx, u.Client, instance)
	updateSts, _, _ := getStateFulSetList(ctx, u.Client, instance)

	pods := &corev1.PodList{}
	_ = u.Client.List(ctx, pods,
		client.InNamespace(instance.Namespace),
		client.MatchingLabels(instance.Labels),
	)

	for _, p := range pods.Items {
		pod := p.DeepCopy()
		controllerRef := metav1.GetControllerOf(pod)
		if controllerRef == nil {
			continue
		}

		onServingCondition := corev1.PodCondition{
			Type:               appsv2beta1.PodOnServing,
			Status:             corev1.ConditionFalse,
			LastProbeTime:      metav1.Now(),
			LastTransitionTime: metav1.Now(),
		}

		if (updateSts != nil && controllerRef.UID == updateSts.UID) ||
			(updateRs != nil && controllerRef.UID == updateRs.UID) {
			for _, condition := range pod.Status.Conditions {
				if condition.Type == corev1.ContainersReady && condition.Status == corev1.ConditionTrue {
					onServingCondition.Status = u.checkInCluster(instance, r, pod)
					break
				}
			}
		}

		patchBytes, _ := json.Marshal(corev1.Pod{
			Status: corev1.PodStatus{
				Conditions: []corev1.PodCondition{onServingCondition},
			},
		})
		_ = u.Client.Status().Patch(ctx, pod.DeepCopy(), client.RawPatch(types.StrategicMergePatchType, patchBytes))
	}
	return subResult{}
}

func (u *updatePodConditions) checkInCluster(instance *appsv2beta1.EMQX, r innerReq.RequesterInterface, pod *corev1.Pod) corev1.ConditionStatus {
	nodes := instance.Status.CoreNodes
	if appsv2beta1.IsExistReplicant(instance) {
		nodes = append(nodes, instance.Status.ReplicantNodes...)
	}
	for _, node := range nodes {
		if pod.UID == node.PodUID {
			if node.Edition == "Enterprise" {
				v, _ := semver.NewVersion(node.Version)
				if v.Compare(semver.MustParse("5.0.3")) >= 0 {
					return u.checkRebalanceStatus(instance, r, pod)
				}
			}
			return corev1.ConditionTrue
		}
	}
	return corev1.ConditionFalse
}

func (u *updatePodConditions) checkRebalanceStatus(instance *appsv2beta1.EMQX, r innerReq.RequesterInterface, pod *corev1.Pod) corev1.ConditionStatus {
	if r == nil {
		return corev1.ConditionFalse
	}
	var port string
	dashboardPort, err := appsv2beta1.GetDashboardServicePort(instance.Spec.Config.Data)
	if err != nil || dashboardPort == nil {
		port = "18083"
	}

	if dashboardPort != nil {
		port = dashboardPort.TargetPort.String()
	}

	requester := &innerReq.Requester{
		Username: r.GetUsername(),
		Password: r.GetPassword(),
		Host:     fmt.Sprintf("%s:%s", pod.Status.PodIP, port),
	}

	url := requester.GetURL("api/v5/load_rebalance/availability_check")
	resp, _, err := requester.Request("GET", url, nil, nil)
	if err != nil {
		return corev1.ConditionUnknown
	}
	if resp.StatusCode != 200 {
		return corev1.ConditionFalse
	}
	return corev1.ConditionTrue
}
