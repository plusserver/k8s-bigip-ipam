package main

import (
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	log "github.com/sirupsen/logrus"

	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	kubernetesinformers "k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/workqueue"

	corev1 "k8s.io/api/core/v1"

	corelisterv1 "k8s.io/client-go/listers/core/v1"

	ipamclientset "github.com/Nexinto/k8s-ipam/pkg/client/clientset/versioned"

	ipamv1 "github.com/Nexinto/k8s-ipam/pkg/apis/ipam.nexinto.com/v1"
	ipaminformers "github.com/Nexinto/k8s-ipam/pkg/client/informers/externalversions"
	ipamlisterv1 "github.com/Nexinto/k8s-ipam/pkg/client/listers/ipam.nexinto.com/v1"
)

type Controller struct {
	Kubernetes        kubernetes.Interface
	KubernetesFactory kubernetesinformers.SharedInformerFactory

	ServiceQueue  workqueue.RateLimitingInterface
	ServiceLister corelisterv1.ServiceLister
	ServiceSynced cache.InformerSynced

	ConfigMapQueue  workqueue.RateLimitingInterface
	ConfigMapLister corelisterv1.ConfigMapLister
	ConfigMapSynced cache.InformerSynced

	IpamClient  ipamclientset.Interface
	IpamFactory ipaminformers.SharedInformerFactory

	IpAddressQueue  workqueue.RateLimitingInterface
	IpAddressLister ipamlisterv1.IpAddressLister
	IpAddressSynced cache.InformerSynced

	Tag        string
	RequireTag bool
	Partition  string
}

// Expects the clientsets to be set.
func (c *Controller) Initialize() {

	if c.Kubernetes == nil {
		panic("c.Kubernetes is nil")
	}
	c.KubernetesFactory = kubernetesinformers.NewSharedInformerFactory(c.Kubernetes, time.Second*30)

	ServiceInformer := c.KubernetesFactory.Core().V1().Services()
	ServiceQueue := workqueue.NewRateLimitingQueue(workqueue.DefaultControllerRateLimiter())
	c.ServiceQueue = ServiceQueue
	c.ServiceLister = ServiceInformer.Lister()
	c.ServiceSynced = ServiceInformer.Informer().HasSynced

	ServiceInformer.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{

		AddFunc: func(obj interface{}) {
			if key, err := cache.MetaNamespaceKeyFunc(obj); err == nil {
				ServiceQueue.Add(key)
			}
		},

		UpdateFunc: func(old, new interface{}) {
			if key, err := cache.MetaNamespaceKeyFunc(new); err == nil {
				ServiceQueue.Add(key)
			}
		},

		DeleteFunc: func(obj interface{}) {
			o, ok := obj.(*corev1.Service)

			if !ok {
				tombstone, ok := obj.(cache.DeletedFinalStateUnknown)
				if !ok {
					log.Errorf("couldn't get object from tombstone %+v", obj)
					return
				}
				o, ok = tombstone.Obj.(*corev1.Service)
				if !ok {
					log.Errorf("tombstone contained object that is not a Service %+v", obj)
					return
				}
			}

			err := c.ServiceDeleted(o)

			if err != nil {
				log.Errorf("failed to process deletion: %s", err.Error())
			}
		},
	})

	ConfigMapInformer := c.KubernetesFactory.Core().V1().ConfigMaps()
	ConfigMapQueue := workqueue.NewRateLimitingQueue(workqueue.DefaultControllerRateLimiter())
	c.ConfigMapQueue = ConfigMapQueue
	c.ConfigMapLister = ConfigMapInformer.Lister()
	c.ConfigMapSynced = ConfigMapInformer.Informer().HasSynced

	ConfigMapInformer.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{

		AddFunc: func(obj interface{}) {
			if key, err := cache.MetaNamespaceKeyFunc(obj); err == nil {
				ConfigMapQueue.Add(key)
			}
		},

		UpdateFunc: func(old, new interface{}) {
			if key, err := cache.MetaNamespaceKeyFunc(new); err == nil {
				ConfigMapQueue.Add(key)
			}
		},
	})

	if c.IpamClient == nil {
		panic("c.IpamClient is nil")
	}
	c.IpamFactory = ipaminformers.NewSharedInformerFactory(c.IpamClient, time.Second*30)

	IpAddressInformer := c.IpamFactory.Ipam().V1().IpAddresses()
	IpAddressQueue := workqueue.NewRateLimitingQueue(workqueue.DefaultControllerRateLimiter())
	c.IpAddressQueue = IpAddressQueue
	c.IpAddressLister = IpAddressInformer.Lister()
	c.IpAddressSynced = IpAddressInformer.Informer().HasSynced

	IpAddressInformer.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{

		UpdateFunc: func(old, new interface{}) {
			if key, err := cache.MetaNamespaceKeyFunc(new); err == nil {
				IpAddressQueue.Add(key)
			}
		},

		DeleteFunc: func(obj interface{}) {
			o, ok := obj.(*ipamv1.IpAddress)

			if !ok {
				tombstone, ok := obj.(cache.DeletedFinalStateUnknown)
				if !ok {
					log.Errorf("couldn't get object from tombstone %+v", obj)
					return
				}
				o, ok = tombstone.Obj.(*ipamv1.IpAddress)
				if !ok {
					log.Errorf("tombstone contained object that is not a IpAddress %+v", obj)
					return
				}
			}

			err := c.IpAddressDeleted(o)

			if err != nil {
				log.Errorf("failed to process deletion: %s", err.Error())
			}
		},
	})

	return
}

func (c *Controller) Start() {
	stopCh := make(chan struct{})
	defer close(stopCh)
	go c.KubernetesFactory.Start(stopCh)
	go c.IpamFactory.Start(stopCh)

	go c.Run(stopCh)

	sigterm := make(chan os.Signal, 1)
	signal.Notify(sigterm, syscall.SIGTERM)
	signal.Notify(sigterm, syscall.SIGINT)
	<-sigterm
}

func (c *Controller) Run(stopCh <-chan struct{}) {

	log.Infof("starting controller")

	defer runtime.HandleCrash()

	defer c.ServiceQueue.ShutDown()
	defer c.ConfigMapQueue.ShutDown()
	defer c.IpAddressQueue.ShutDown()

	if !cache.WaitForCacheSync(stopCh, c.ServiceSynced, c.ConfigMapSynced, c.IpAddressSynced) {
		runtime.HandleError(fmt.Errorf("Timed out waiting for caches to sync"))
		return
	}

	log.Debugf("starting workers")

	go wait.Until(c.runServiceWorker, time.Second, stopCh)

	go wait.Until(c.runConfigMapWorker, time.Second, stopCh)

	go wait.Until(c.runIpAddressWorker, time.Second, stopCh)

	log.Debugf("started workers")
	<-stopCh
	log.Debugf("shutting down workers")
}

func (c *Controller) runServiceWorker() {
	for c.processNextService() {
	}
}

func (c *Controller) processNextService() bool {
	obj, shutdown := c.ServiceQueue.Get()
	if shutdown {
		return false
	}

	err := func(obj interface{}) error {
		defer c.ServiceQueue.Done(obj)
		var key string
		var ok bool

		if key, ok = obj.(string); !ok {
			c.ServiceQueue.Forget(obj)
			runtime.HandleError(fmt.Errorf("expected string in workqueue but got %#v", obj))
			return nil
		}

		if err := c.processService(key); err != nil {
			return fmt.Errorf("error syncing '%s': %s", key, err.Error())
		}

		c.ServiceQueue.Forget(obj)
		return nil
	}(obj)

	if err != nil {
		runtime.HandleError(err)
		return true
	}

	return true
}

func (c *Controller) processService(key string) error {

	namespace, name, err := cache.SplitMetaNamespaceKey(key)
	if err != nil {
		return fmt.Errorf("could not parse name %s: %s", key, err.Error())
	}

	o, err := c.ServiceLister.Services(namespace).Get(name)
	if err != nil {
		if errors.IsNotFound(err) {
			return fmt.Errorf("tried to get %s, but it was not found", key)
		} else {
			return fmt.Errorf("error getting %s from cache: %s", key, err.Error())
		}
	}

	return c.ServiceCreatedOrUpdated(o)

}

func (c *Controller) runConfigMapWorker() {
	for c.processNextConfigMap() {
	}
}

func (c *Controller) processNextConfigMap() bool {
	obj, shutdown := c.ConfigMapQueue.Get()
	if shutdown {
		return false
	}

	err := func(obj interface{}) error {
		defer c.ConfigMapQueue.Done(obj)
		var key string
		var ok bool

		if key, ok = obj.(string); !ok {
			c.ConfigMapQueue.Forget(obj)
			runtime.HandleError(fmt.Errorf("expected string in workqueue but got %#v", obj))
			return nil
		}

		if err := c.processConfigMap(key); err != nil {
			return fmt.Errorf("error syncing '%s': %s", key, err.Error())
		}

		c.ConfigMapQueue.Forget(obj)
		return nil
	}(obj)

	if err != nil {
		runtime.HandleError(err)
		return true
	}

	return true
}

func (c *Controller) processConfigMap(key string) error {

	namespace, name, err := cache.SplitMetaNamespaceKey(key)
	if err != nil {
		return fmt.Errorf("could not parse name %s: %s", key, err.Error())
	}

	o, err := c.ConfigMapLister.ConfigMaps(namespace).Get(name)
	if err != nil {
		if errors.IsNotFound(err) {
			return fmt.Errorf("tried to get %s, but it was not found", key)
		} else {
			return fmt.Errorf("error getting %s from cache: %s", key, err.Error())
		}
	}

	return c.ConfigMapCreatedOrUpdated(o)

}

func (c *Controller) runIpAddressWorker() {
	for c.processNextIpAddress() {
	}
}

func (c *Controller) processNextIpAddress() bool {
	obj, shutdown := c.IpAddressQueue.Get()
	if shutdown {
		return false
	}

	err := func(obj interface{}) error {
		defer c.IpAddressQueue.Done(obj)
		var key string
		var ok bool

		if key, ok = obj.(string); !ok {
			c.IpAddressQueue.Forget(obj)
			runtime.HandleError(fmt.Errorf("expected string in workqueue but got %#v", obj))
			return nil
		}

		if err := c.processIpAddress(key); err != nil {
			return fmt.Errorf("error syncing '%s': %s", key, err.Error())
		}

		c.IpAddressQueue.Forget(obj)
		return nil
	}(obj)

	if err != nil {
		runtime.HandleError(err)
		return true
	}

	return true
}

func (c *Controller) processIpAddress(key string) error {

	namespace, name, err := cache.SplitMetaNamespaceKey(key)
	if err != nil {
		return fmt.Errorf("could not parse name %s: %s", key, err.Error())
	}

	o, err := c.IpAddressLister.IpAddresses(namespace).Get(name)
	if err != nil {
		if errors.IsNotFound(err) {
			return fmt.Errorf("tried to get %s, but it was not found", key)
		} else {
			return fmt.Errorf("error getting %s from cache: %s", key, err.Error())
		}
	}

	return c.IpAddressCreatedOrUpdated(o)

}
