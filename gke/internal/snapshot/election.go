package snapshot

import (
	"context"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/leaderelection"
	"k8s.io/client-go/tools/leaderelection/resourcelock"
)

// runElected runs work while this replica holds the addon Lease. When leadership is
// lost the workers stop but the process keeps serving admission requests and
// immediately campaigns again, so a lease blip never takes the webhooks down.
func (c *Controller) runElected(ctx context.Context, work func(context.Context)) error {
	if !c.leaderElection {
		work(ctx)
		return nil
	}
	lock := &resourcelock.LeaseLock{
		LeaseMeta: metav1.ObjectMeta{Name: c.leaseName, Namespace: c.leaseNamespace},
		Client:    c.kube.CoordinationV1(),
		LockConfig: resourcelock.ResourceLockConfig{
			Identity: c.identity,
		},
	}
	for {
		elector, err := leaderelection.NewLeaderElector(leaderelection.LeaderElectionConfig{
			Lock:            lock,
			LeaseDuration:   15 * time.Second,
			RenewDeadline:   10 * time.Second,
			RetryPeriod:     2 * time.Second,
			ReleaseOnCancel: true,
			Name:            c.leaseName,
			Callbacks: leaderelection.LeaderCallbacks{
				OnStartedLeading: func(leaderCtx context.Context) {
					logf("reconciler", "acquired lease %s/%s; this replica is the active reconciler", c.leaseNamespace, c.leaseName)
					work(leaderCtx)
				},
				OnStoppedLeading: func() {
					logf("reconciler", "lost lease %s/%s; pausing reconciliation while still serving webhooks", c.leaseNamespace, c.leaseName)
				},
				OnNewLeader: func(identity string) {
					if identity != c.identity {
						logf("reconciler", "replica %s is the active reconciler", identity)
					}
				},
			},
		})
		if err != nil {
			return err
		}
		elector.Run(ctx)
		if ctx.Err() != nil {
			return nil
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(2 * time.Second):
		}
	}
}
