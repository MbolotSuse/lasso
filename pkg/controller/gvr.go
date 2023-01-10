package controller

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"k8s.io/apiextensions-apiserver/pkg/client/clientset/clientset"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/discovery"
	"k8s.io/klog"
)

type gvrsCallback func([]schema.GroupVersionResource) error

type gvrWatcher struct {
	sync.Mutex

	toSync int32
	//TODO: More info on how this works
	//TODO: Race conditions
	//TODO: Chain remove CRD? Delete one -> Delete two
	client   discovery.DiscoveryInterface
	callback gvrsCallback
}

func (s *sharedControllerFactory) watchGVKS(ctx context.Context,
	callback gvrsCallback) error {
	clientSet, err := clientset.NewForConfig(s.cfg)
	if err != nil {
		return err
	}
	h := &gvrWatcher{
		client:   clientSet.Discovery(),
		callback: callback,
	}

	crdController, err := s.ForKind(schema.GroupVersionKind{
		Group:   "apiextensions.k8s.io",
		Version: "v1",
		Kind:    "CustomResourceDefinition",
	})
	if err != nil {
		return err
	}

	apiServiceController, err := s.ForKind(schema.GroupVersionKind{
		Group:   "apiregistration.k8s.io",
		Version: "v1",
		Kind:    "APIService",
	})

	if err != nil {
		return err
	}

	crdController.RegisterHandler(ctx, "dynamic-types", h)
	apiServiceController.RegisterHandler(ctx, "dynamic-types", h)
	return nil
}

func (g *gvrWatcher) OnChange(_ string, obj runtime.Object) (runtime.Object, error) {
	g.queueRefresh()
	return obj, nil
}

func (g *gvrWatcher) queueRefresh() {
	atomic.StoreInt32(&g.toSync, 1)

	go func() {
		time.Sleep(500 * time.Millisecond)
		if err := g.refreshAll(); err != nil {
			klog.Errorf("failed to sync schemas: %v", err)
			atomic.StoreInt32(&g.toSync, 1)
		}
	}()
}

func (g *gvrWatcher) getGVRs() (result []schema.GroupVersionResource, _ error) {
	_, resources, err := g.client.ServerGroupsAndResources()
	if err != nil {
		return nil, err
	}
	for _, resource := range resources {
		for _, apiResource := range resource.APIResources {
			groupVersion, err := schema.ParseGroupVersion(resource.GroupVersion)
			if err != nil {
				return nil, fmt.Errorf("unable to get new GVRs: %w", err)
			}
			gvr := schema.GroupVersionResource{
				Group:    groupVersion.Group,
				Version:  groupVersion.Version,
				Resource: apiResource.Name,
			}
			// resource.GroupVersion?
			result = append(result, gvr)
		}
	}
	return result, nil
}

func (g *gvrWatcher) refreshAll() error {
	g.Lock()
	defer g.Unlock()

	if !g.needToSync() {
		return nil
	}

	gvks, err := g.getGVRs()
	if err != nil {
		return err
	}

	return g.callback(gvks)
}

func (g *gvrWatcher) needToSync() bool {
	old := atomic.SwapInt32(&g.toSync, 0)
	return old == 1
}
