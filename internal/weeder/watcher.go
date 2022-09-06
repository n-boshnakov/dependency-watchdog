package weeder

import (
	"context"
	"fmt"
	"github.com/gardener/dependency-watchdog/internal/util"
	"github.com/go-logr/logr"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/kubernetes"
	"time"
)

const watchCreationRetryInterval = 500 * time.Millisecond

type podEventHandler func(ctx context.Context, log logr.Logger, apiClient kubernetes.Interface, podNamespaceName types.NamespacedName) error

// podWatcher watches a pod for status changes
type podWatcher struct {
	selector       *metav1.LabelSelector
	eventHandlerFn podEventHandler
	k8sWatch       watch.Interface
	weeder         *Weeder
	log            logr.Logger
}

func (pw *podWatcher) createK8sWatch(ctx context.Context) {
	operation := fmt.Sprintf("Creating kubernetes watch for namespace %s, service %s with selector %s", pw.weeder.namespace, pw.weeder.endpoints.Name, pw.selector)
	util.RetryOnError(ctx, operation, func() error {
		w, err := doCreateK8sWatch(ctx, pw.weeder.watchClient, pw.weeder.namespace, pw.selector)
		if err != nil {
			return err
		}
		pw.k8sWatch = w
		return nil
	}, watchCreationRetryInterval)
}

func (pw *podWatcher) close() {
	if pw.k8sWatch != nil {
		pw.k8sWatch.Stop()
	}
}

func doCreateK8sWatch(ctx context.Context, client kubernetes.Interface, namespace string, lSelector *metav1.LabelSelector) (watch.Interface, error) {
	selector, err := metav1.LabelSelectorAsSelector(lSelector)
	w, err := client.CoreV1().Pods(namespace).Watch(ctx, metav1.ListOptions{
		LabelSelector: selector.String(),
	})
	if err != nil {
		return nil, err
	}
	return w, nil
}

func (pw *podWatcher) watch() {
	defer pw.close()
	pw.createK8sWatch(pw.weeder.ctx)
	for {
		select {
		case <-pw.weeder.ctx.Done():
			pw.log.V(4).Info("Exiting watch as context has timed-out or has been cancelled", "namespace", pw.weeder.namespace, "selector", pw.selector.String())
			return
		case event, ok := <-pw.k8sWatch.ResultChan():
			if !ok {
				pw.log.V(3).Info("Watch has stopped, recreating kubernetes watch", "namespace", pw.weeder.namespace, "service", pw.weeder.endpoints.Name, "selector", pw.selector, pw.selector.String())
				pw.createK8sWatch(pw.weeder.ctx)
				continue
			}
			if !canProcessEvent(event) {
				continue
			}
			targetPod := event.Object.(*v1.Pod)
			if err := pw.eventHandlerFn(pw.weeder.ctx, pw.log, pw.weeder.watchClient, types.NamespacedName{Namespace: targetPod.Namespace, Name: targetPod.Name}); err != nil {
				pw.log.Error(err, "error processing pod ", "podName", targetPod.Name)
			}
		}
	}
}

func canProcessEvent(ev watch.Event) bool {
	if ev.Type == watch.Added || ev.Type == watch.Modified {
		switch ev.Object.(type) {
		case *v1.Pod:
			return true
		default:
			return false
		}
	}
	return false
}