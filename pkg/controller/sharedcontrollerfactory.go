package controller

import (
	"context"
	"fmt"
	"sync"

	"github.com/rancher/lasso/pkg/cache"
	"github.com/rancher/lasso/pkg/client"
	"github.com/sirupsen/logrus"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/util/workqueue"
)

type SharedControllerFactory interface {
	ForObject(obj runtime.Object) (SharedController, error)
	ForKind(gvk schema.GroupVersionKind) (SharedController, error)
	ForResource(gvr schema.GroupVersionResource, namespaced bool) SharedController
	ForResourceKind(gvr schema.GroupVersionResource, kind string, namespaced bool) SharedController
	SharedCacheFactory() cache.SharedCacheFactory
	Start(ctx context.Context, workers int) error
}

type SharedControllerFactoryOptions struct {
	CacheOptions *cache.SharedCacheFactoryOptions

	DefaultRateLimiter workqueue.RateLimiter
	DefaultWorkers     int

	KindRateLimiter map[schema.GroupVersionKind]workqueue.RateLimiter
	KindWorkers     map[schema.GroupVersionKind]int

	// SyncOnlyChangedObjects causes the handle function to only proceed if the object was actually updated.
	// This is intended to be used by applications with many objects and/or controllers types that have
	// alternative means of rerunning when necessary. When the informer's resync their cache the update
	// function is run. If this setting is enabled, when the update handler is triggered the overhead is
	// reduced but has the tradeoff of not rerunning handlers. Handlers that rely on external objects or
	// services, or experience a bug might need to rerun despite the respective object not changing. If this
	// is enabled, it is the responsibility of the app to ensure logic is retried when needed. The result is
	// that running the handler func on resync will mostly only serve the purpose of catching missed cache
	// events.
	SyncOnlyChangedObjects bool
}

type sharedControllerFactory struct {
	controllerLock sync.RWMutex

	sharedCacheFactory cache.SharedCacheFactory
	controllers        map[schema.GroupVersionResource]*sharedController
	// controllerCancels holds the functions to cancel the contexts for the controllers
	controllerCancels map[schema.GroupVersionResource]func()
	cfg               *rest.Config

	rateLimiter     workqueue.RateLimiter
	workers         int
	kindRateLimiter map[schema.GroupVersionKind]workqueue.RateLimiter
	kindWorkers     map[schema.GroupVersionKind]int

	syncOnlyChangedObjects bool
}

func NewSharedControllerFactoryFromConfig(config *rest.Config, scheme *runtime.Scheme) (SharedControllerFactory, error) {
	cf, err := client.NewSharedClientFactory(config, &client.SharedClientFactoryOptions{
		Scheme: scheme,
	})
	if err != nil {
		return nil, err
	}
	return NewSharedControllerFactory(cache.NewSharedCachedFactory(cf, nil), nil), nil
}

// NewSharedControllerFactoryFromConfigWithOptions accepts options for configuring a new SharedControllerFactory and its
// cache.
func NewSharedControllerFactoryFromConfigWithOptions(config *rest.Config, scheme *runtime.Scheme, opts *SharedControllerFactoryOptions) (SharedControllerFactory, error) {
	cf, err := client.NewSharedClientFactory(config, &client.SharedClientFactoryOptions{
		Scheme: scheme,
	})
	if err != nil {
		return nil, err
	}
	var cacheOpts *cache.SharedCacheFactoryOptions
	if opts != nil {
		cacheOpts = opts.CacheOptions
	}
	return NewSharedControllerFactory(cache.NewSharedCachedFactory(cf, cacheOpts), opts), nil
}

func NewSharedControllerFactory(cacheFactory cache.SharedCacheFactory, opts *SharedControllerFactoryOptions) SharedControllerFactory {
	opts = applyDefaultSharedOptions(opts)
	return &sharedControllerFactory{
		sharedCacheFactory:     cacheFactory,
		controllers:            map[schema.GroupVersionResource]*sharedController{},
		controllerCancels:      map[schema.GroupVersionResource]func(){},
		cfg:                    cacheFactory.SharedClientFactory().RestConfig(),
		workers:                opts.DefaultWorkers,
		kindWorkers:            opts.KindWorkers,
		rateLimiter:            opts.DefaultRateLimiter,
		kindRateLimiter:        opts.KindRateLimiter,
		syncOnlyChangedObjects: opts.SyncOnlyChangedObjects,
	}
}

func applyDefaultSharedOptions(opts *SharedControllerFactoryOptions) *SharedControllerFactoryOptions {
	var newOpts SharedControllerFactoryOptions
	if opts != nil {
		newOpts = *opts
	}
	if newOpts.DefaultWorkers == 0 {
		newOpts.DefaultWorkers = 5
	}
	return &newOpts
}

func (s *sharedControllerFactory) EnableSyncOnlyChangedObjects() {
	s.syncOnlyChangedObjects = true
}

func (s *sharedControllerFactory) Start(ctx context.Context, defaultWorkers int) error {
	s.controllerLock.Lock()
	defer s.controllerLock.Unlock()

	if err := s.sharedCacheFactory.Start(ctx); err != nil {
		return err
	}

	// copy so we can release the lock during cache wait
	controllersCopy := map[schema.GroupVersionResource]*sharedController{}
	for k, v := range s.controllers {
		controllersCopy[k] = v
	}

	// Do not hold lock while waiting because this can cause a deadlock if
	// one of the handlers you are waiting on tries to acquire this lock (by looking up
	// shared controller)
	s.controllerLock.Unlock()
	s.sharedCacheFactory.WaitForCacheSync(ctx)
	s.controllerLock.Lock()

	for gvr, controller := range controllersCopy {
		w, err := s.getWorkers(gvr, defaultWorkers)
		if err != nil {
			return err
		}
		newCtx, cancelFunc := context.WithCancel(ctx)
		s.controllerCancels[gvr] = cancelFunc
		if err := controller.Start(newCtx, w); err != nil {
			return err
		}
	}
	// unlock to watch for the GVKs
	s.controllerLock.Unlock()
	// re-lock so that the deferred unlock doesn't panic
	defer s.controllerLock.Lock()
	err := s.watchGVKS(ctx, s.updateControllers)
	return err
}

func (s *sharedControllerFactory) ForObject(obj runtime.Object) (SharedController, error) {
	gvk, err := s.sharedCacheFactory.SharedClientFactory().GVKForObject(obj)
	if err != nil {
		return nil, err
	}
	return s.ForKind(gvk)
}

func (s *sharedControllerFactory) ForKind(gvk schema.GroupVersionKind) (SharedController, error) {
	gvr, nsed, err := s.sharedCacheFactory.SharedClientFactory().ResourceForGVK(gvk)
	if err != nil {
		return nil, err
	}

	return s.ForResourceKind(gvr, gvk.Kind, nsed), nil
}

func (s *sharedControllerFactory) ForResource(gvr schema.GroupVersionResource, namespaced bool) SharedController {
	return s.ForResourceKind(gvr, "", namespaced)
}

func (s *sharedControllerFactory) ForResourceKind(gvr schema.GroupVersionResource, kind string, namespaced bool) SharedController {
	controllerResult := s.byResource(gvr)
	if controllerResult != nil {
		return controllerResult
	}

	s.controllerLock.Lock()
	defer s.controllerLock.Unlock()

	controllerResult = s.controllers[gvr]
	if controllerResult != nil {
		return controllerResult
	}

	client := s.sharedCacheFactory.SharedClientFactory().ForResourceKind(gvr, kind, namespaced)

	handler := &SharedHandler{controllerGVR: gvr.String()}

	controllerResult = &sharedController{
		deferredController: func() (Controller, error) {
			var (
				gvk schema.GroupVersionKind
				err error
			)

			if kind == "" {
				gvk, err = s.sharedCacheFactory.SharedClientFactory().GVKForResource(gvr)
				if err != nil {
					return nil, err
				}
			} else {
				gvk = gvr.GroupVersion().WithKind(kind)
			}

			cache, err := s.sharedCacheFactory.ForResourceKind(gvr, kind, namespaced)
			if err != nil {
				return nil, err
			}

			rateLimiter, ok := s.kindRateLimiter[gvk]
			if !ok {
				rateLimiter = s.rateLimiter
			}

			starter := func(ctx context.Context) error {
				return s.sharedCacheFactory.StartGVK(ctx, gvk)
			}

			c := New(gvk.String(), cache, starter, handler, &Options{
				RateLimiter:            rateLimiter,
				SyncOnlyChangedObjects: s.syncOnlyChangedObjects,
			})

			return c, err
		},
		handler: handler,
		client:  client,
	}

	s.controllers[gvr] = controllerResult
	return controllerResult
}

func (s *sharedControllerFactory) getWorkers(gvr schema.GroupVersionResource, workers int) (int, error) {
	gvk, err := s.sharedCacheFactory.SharedClientFactory().GVKForResource(gvr)
	if meta.IsNoMatchError(err) {
		return workers, nil
	} else if err != nil {
		return 0, err
	}

	w, ok := s.kindWorkers[gvk]
	if ok {
		return w, nil
	}
	if workers > 0 {
		return workers, nil
	}
	return s.workers, nil
}

func (s *sharedControllerFactory) byResource(gvr schema.GroupVersionResource) *sharedController {
	s.controllerLock.RLock()
	defer s.controllerLock.RUnlock()
	return s.controllers[gvr]
}

func (s *sharedControllerFactory) SharedCacheFactory() cache.SharedCacheFactory {
	return s.sharedCacheFactory
}

// updateControllers cancels/removes old controllers which are for removed api groups.
func (s *sharedControllerFactory) updateControllers(newGVRs []schema.GroupVersionResource) error {
	s.controllerLock.Lock()
	defer s.controllerLock.Unlock()
	// convert to a map for easy lookup. TODO: Change input to a map to not do this here?
	lookup := map[schema.GroupVersionResource]struct{}{}
	for _, gvr := range newGVRs {
		lookup[gvr] = struct{}{}
	}

	// identify GVRs which are no longer present. Record in separate list to avoid changing map during iteration
	var removeGVRs []schema.GroupVersionResource
	for oldGVR, _ := range s.controllerCancels {
		if _, ok := lookup[oldGVR]; !ok {
			removeGVRs = append(removeGVRs, oldGVR)
		}
	}

	for _, removeGVR := range removeGVRs {
		cancelFunc, ok := s.controllerCancels[removeGVR]
		if !ok {
			return fmt.Errorf("unable to cancel context for GVR %s", removeGVR.String())
		}
		// cancelFunc instructs the controller to gracefully shut down. Once shut down, we remove from the map
		logrus.Errorf("new GVRs: %v", newGVRs)
		logrus.Infof("stopping controllers for GVR %s", removeGVR.String())
		cancelFunc()
		delete(s.controllerCancels, removeGVR)
		delete(s.controllers, removeGVR)
	}

	return nil
}
