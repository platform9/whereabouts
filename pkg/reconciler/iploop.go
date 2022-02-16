package reconciler

import (
	"context"
	"fmt"
	"net"
	"strings"
	"time"

	"github.com/dougbtv/whereabouts/pkg/allocate"
	whereaboutsv1alpha1 "github.com/dougbtv/whereabouts/pkg/api/v1alpha1"
	"github.com/dougbtv/whereabouts/pkg/logging"
	"github.com/dougbtv/whereabouts/pkg/storage"
	"github.com/dougbtv/whereabouts/pkg/storage/kubernetes"
	"github.com/dougbtv/whereabouts/pkg/types"

	v1 "k8s.io/api/core/v1"
)

type ReconcileLooper struct {
	ctx                    context.Context
	k8sClient              kubernetes.Client
	liveWhereaboutsPods    map[string]podWrapper
	orphanedIPs            []OrphanedIPReservations
	orphanedClusterWideIPs []whereaboutsv1alpha1.OverlappingRangeIPReservation
}

type OrphanedIPReservations struct {
	Pool        storage.IPPool
	Allocations []types.IPReservation
}

func NewReconcileLooper(kubeConfigPath string, ctx context.Context) (*ReconcileLooper, error) {
	logging.Debugf("NewReconcileLooper - Kubernetes config file located at: %s", kubeConfigPath)
	k8sClient, err := kubernetes.NewClient(kubeConfigPath)
	if err != nil {
		return nil, logging.Errorf("failed to instantiate the Kubernetes client: %+v", err)
	}
	logging.Debugf("successfully read the kubernetes configuration file located at: %s", kubeConfigPath)

	pods, err := k8sClient.ListPods()
	if err != nil {
		return nil, err
	}

	ipPools, err := k8sClient.ListIPPools(ctx)
	if err != nil {
		return nil, logging.Errorf("failed to retrieve all IP pools: %v", err)
	}

	whereaboutsPodRefs := getPodRefsServedByWhereabouts(ipPools)
	looper := &ReconcileLooper{
		ctx:                 ctx,
		k8sClient:           *k8sClient,
		liveWhereaboutsPods: indexPods(pods, whereaboutsPodRefs),
	}

	if err := looper.findOrphanedIPsPerPool(ipPools); err != nil {
		return nil, err
	}

	if err := looper.findClusterWideIPReservations(); err != nil {
		return nil, err
	}
	return looper, nil
}

func (rl *ReconcileLooper) findOrphanedIPsPerPool(ipPools []storage.IPPool) error {
	for _, pool := range ipPools {
		orphanIP := OrphanedIPReservations{
			Pool: pool,
		}
		for _, ipReservation := range pool.Allocations() {
			logging.Debugf("the IP reservation: %s", ipReservation)
			if ipReservation.PodRef == "" {
				_ = logging.Errorf("pod ref missing for Allocations: %s", ipReservation)
				continue
			}
			if !rl.isPodAlive(ipReservation.PodRef, ipReservation.IP.String()) {
				logging.Debugf("pod ref %s is not listed in the live pods list", ipReservation.PodRef)
				orphanIP.Allocations = append(orphanIP.Allocations, ipReservation)
			}
		}
		if len(orphanIP.Allocations) > 0 {
			rl.orphanedIPs = append(rl.orphanedIPs, orphanIP)
		}
	}

	return nil
}

func (rl ReconcileLooper) isPodAlive(podRef string, ip string) bool {
	for livePodRef, livePod := range rl.liveWhereaboutsPods {
		if podRef == livePodRef {
			isFound := isIpOnPod(&livePod, podRef, ip)
			if !isFound && (livePod.phase == v1.PodPending) {
				/* Sometimes pods are still coming up, and may not yet have Multus
				 * annotation added to it yet. We don't want to check the IPs yet
				 * so re-fetch the Pod 5x
				 */
				podToMatch := &livePod
				retries := 0

				logging.Debugf("Re-fetching Pending Pod: %s IP-to-match: %s", livePodRef, ip)

				for retries < storage.PodRefreshRetries {
					retries += 1
					podToMatch = rl.refreshPod(livePodRef)
					if podToMatch == nil {
						logging.Debugf("Cleaning up...")
						return false
					} else if podToMatch.phase != v1.PodPending {
						logging.Debugf("Pending Pod is now in phase: %s", podToMatch.phase)
						break
					} else {
						isFound = isIpOnPod(podToMatch, podRef, ip)
						// Short-circuit - Pending Pod may have IP now
						if isFound {
							logging.Debugf("Pod now has IP annotation while in Pending")
							return true
						}
						time.Sleep(time.Duration(500) * time.Millisecond)
					}
				}
				isFound = isIpOnPod(podToMatch, podRef, ip)
			}

			return isFound
		}
	}
	return false
}

func (rl ReconcileLooper) refreshPod(podRef string) *podWrapper {
	namespace, podName := splitPodRef(podRef)
	if namespace == "" || podName == "" {
		logging.Errorf("Invalid podRef format: %s", podRef)
		return nil
	}

	pod, err := rl.k8sClient.GetPod(namespace, podName)
	if err != nil {
		logging.Errorf("Failed to refresh Pod %s: %s\n", podRef, err)
		return nil
	}

	wrappedPod := wrapPod(*pod)
	logging.Debugf("Got refreshed pod: %v", wrappedPod)
	return wrappedPod
}

func splitPodRef(podRef string) (string, string) {
	namespacedName := strings.Split(podRef, "/")
	if len(namespacedName) != 2 {
		logging.Errorf("Failed to split podRef %s", podRef)
		return "", ""
	}

	return namespacedName[0], namespacedName[1]
}

func composePodRef(pod v1.Pod) string {
	return fmt.Sprintf("%s/%s", pod.GetNamespace(), pod.GetName())
}

func (rl ReconcileLooper) ReconcileIPPools() ([]net.IP, error) {
	matchByContainerID := func(reservations []types.IPReservation, cid string) int {
		foundidx := -1
		for idx, v := range reservations {
			if v.ContainerID == cid {
				return idx
			}
		}
		return foundidx
	}

	var err error
	var totalCleanedUpIps []net.IP
	for _, orphanedIP := range rl.orphanedIPs {
		currentIPReservations := orphanedIP.Pool.Allocations()
		containerIDsToDeallocate := findOutContainerIDsToDeallocateIPsFrom(orphanedIP)
		var deallocatedIP net.IP
		for _, cid := range containerIDsToDeallocate {
			currentIPReservations, deallocatedIP, err = allocate.IterateForDeallocation(currentIPReservations, cid, matchByContainerID)
			if err != nil {
				return nil, err
			}
		}

		logging.Debugf("Going to update the reserve list to: %+v", currentIPReservations)
		if err := orphanedIP.Pool.Update(context.TODO(), currentIPReservations); err != nil {
			return nil, logging.Errorf("failed to update the reservation list: %v", err)
		}
		totalCleanedUpIps = append(totalCleanedUpIps, deallocatedIP)
	}

	return totalCleanedUpIps, nil
}

func (rl *ReconcileLooper) findClusterWideIPReservations() error {
	clusterWideIPReservations, err := rl.k8sClient.ListOverlappingIPs(rl.ctx)
	if err != nil {
		return logging.Errorf("failed to list all OverLappingIPs: %v", err)
	}

	for _, clusterWideIPReservation := range clusterWideIPReservations {
		ip := clusterWideIPReservation.GetName()
		podRef := clusterWideIPReservation.Spec.PodRef

		if !rl.isPodAlive(podRef, ip) {
			logging.Debugf("pod ref %s is not listed in the live pods list", podRef)
			rl.orphanedClusterWideIPs = append(rl.orphanedClusterWideIPs, clusterWideIPReservation)
		}
	}

	return nil
}

func (rl ReconcileLooper) ReconcileOverlappingIPAddresses() error {
	var failedReconciledClusterWideIPs []string
	for _, overlappingIPStruct := range rl.orphanedClusterWideIPs {
		if err := rl.k8sClient.DeleteOverlappingIP(rl.ctx, &overlappingIPStruct); err != nil {
			logging.Errorf("failed to remove cluster wide IP: %s", overlappingIPStruct.GetName())
			failedReconciledClusterWideIPs = append(failedReconciledClusterWideIPs, overlappingIPStruct.GetName())
			continue
		}
		logging.Debugf("removed stale overlappingIP allocation [%s]", overlappingIPStruct.GetName())
	}

	if len(failedReconciledClusterWideIPs) != 0 {
		return logging.Errorf("could not reconcile cluster wide IPs: %v", failedReconciledClusterWideIPs)
	}
	return nil
}

func findOutContainerIDsToDeallocateIPsFrom(orphanedIP OrphanedIPReservations) []string {
	var cidsToDeallocate []string
	for _, orphanedAllocation := range orphanedIP.Allocations {
		cidsToDeallocate = append(cidsToDeallocate, orphanedAllocation.ContainerID)
	}
	return cidsToDeallocate
}
