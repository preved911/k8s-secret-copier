package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/spf13/pflag"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	runtime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/workqueue"
	"sigs.k8s.io/controller-runtime/pkg/client/config"
)

var (
	srcNamespacedName     *string
	dstNamespacedName     *[]string
	srcNamespace, srcName string
	clientset             *kubernetes.Clientset
)

func main() {
	srcNamespacedName = pflag.String("src-namespaced-name", "", "Source secret name in format namespace/name")
	dstNamespacedName = pflag.StringSlice("dst-namespaced-name", []string{}, "Destination secret name in format namespace/name(can be given multiple times)")
	pflag.Parse()

	log.SetFlags(log.Ldate | log.Ltime | log.Llongfile)

	if *srcNamespacedName == "" {
		log.Fatalln("source secret name should be specified")
	}

	if len(*dstNamespacedName) == 0 {
		log.Fatalln("at least one destination secret name should be specified")
	}

	conf, err := config.GetConfig()
	if err != nil {
		log.Fatalf("failed to get kubeconfig: %s", err)
	}

	clientset, err = kubernetes.NewForConfig(conf)
	if err != nil {
		log.Fatalf("failed to init clientset: %s", err)
	}

	srcNamespace, srcName, err = getNamespaceAndName(*srcNamespacedName)
	if err != nil {
		log.Fatalf("failed to parse source secret name: %s", err)
	}

	watcher := cache.NewListWatchFromClient(clientset.CoreV1().RESTClient(), "secrets", "", fields.Everything())
	queue := workqueue.NewRateLimitingQueue(workqueue.DefaultControllerRateLimiter())
	indexer, informer := cache.NewIndexerInformer(watcher, &v1.Secret{}, 0, cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			key, err := proccessItem(obj)
			if err == nil {
				queue.Add(key)
			}
		},
		UpdateFunc: func(old interface{}, new interface{}) {
			key, err := proccessItem(new)
			if err == nil {
				queue.Add(key)
			}
		},
		DeleteFunc: func(obj interface{}) {
			key, err := proccessItem(obj)
			if err == nil {
				queue.Add(key)
			}
		},
	}, cache.Indexers{})

	controller := NewController(queue, indexer, informer)

	stop := make(chan struct{})
	defer close(stop)

	controller.Run(1, stop)
}

func getNamespaceAndName(namespacedName string) (string, string, error) {
	v := strings.Split(namespacedName, "/")
	if len(v) != 2 {
		return "", "", errors.New("incorrect flag value")
	}

	return v[0], v[1], nil
}

func proccessItem(obj interface{}) (string, error) {
	key, err := cache.MetaNamespaceKeyFunc(obj)
	if err != nil {
		return "", err
	}

	if key == *srcNamespacedName || key == srcName {
		return key, nil
	}

	for _, dst := range *dstNamespacedName {
		_, dstName, err := getNamespaceAndName(dst)
		if err != nil {
			log.Printf("failed to parse destination secret name: %s", err)
		}

		if key == dst || key == dstName {
			return key, nil
		}
	}

	return "", errors.New("given key not found")
}

func (c *Controller) updateSecrets(key string) error {
	_, exists, err := c.indexer.GetByKey(key)
	if err != nil {
		log.Println("fetching object with key %s from store failed with %v", key, err)
		return err
	}

	if !exists {
		log.Printf("secret %s does not exist anymore", key)
	}

	srcSecret, err := clientset.CoreV1().Secrets(srcNamespace).Get(context.TODO(), srcName, metav1.GetOptions{})
	if err != nil {
		log.Printf("failed to get source secret: %s", err)

		return nil
	}

	for _, dst := range *dstNamespacedName {
		log.Printf("updating %s", dst)

		dstNamespace, dstName, err := getNamespaceAndName(dst)
		if err != nil {
			log.Printf("failed to parse destination secret name: %s", err)
		}

		dstSecret := &v1.Secret{
			ObjectMeta: metav1.ObjectMeta{
				Name:      dstName,
				Namespace: dstNamespace,
			},
			Data: srcSecret.Data,
		}
		_, err = clientset.CoreV1().Secrets(dstNamespace).Update(
			context.TODO(),
			dstSecret,
			metav1.UpdateOptions{FieldManager: "preved911/k8s-secret-copier"},
		)
		if err != nil {
			log.Printf("failed to update secret: %s", err)

			log.Printf("creating %s", dst)

			_, err = clientset.CoreV1().Secrets(dstNamespace).Create(
				context.TODO(),
				dstSecret,
				metav1.CreateOptions{FieldManager: "preved911/k8s-secret-copier"},
			)
			if err != nil {
				log.Printf("failed to create secret: %s", err)
			}
		}
	}

	return nil
}

// Controller demonstrates how to implement a controller with client-go.
type Controller struct {
	indexer  cache.Indexer
	queue    workqueue.RateLimitingInterface
	informer cache.Controller
}

// NewController creates a new Controller.
func NewController(queue workqueue.RateLimitingInterface, indexer cache.Indexer, informer cache.Controller) *Controller {
	return &Controller{
		informer: informer,
		indexer:  indexer,
		queue:    queue,
	}
}

// Run begins watching and syncing.
func (c *Controller) Run(workers int, stopCh chan struct{}) {
	defer runtime.HandleCrash()

	// Let the workers stop when we are done
	defer c.queue.ShutDown()
	log.Println("starting secret controller")

	go c.informer.Run(stopCh)

	// Wait for all involved caches to be synced, before processing items from the queue is started
	if !cache.WaitForCacheSync(stopCh, c.informer.HasSynced) {
		runtime.HandleError(fmt.Errorf("timed out waiting for caches to sync"))
		return
	}

	for i := 0; i < workers; i++ {
		go wait.Until(c.runWorker, time.Second, stopCh)
	}

	<-stopCh
	log.Println("stopping secret controller")
}

func (c *Controller) runWorker() {
	for c.processNextItem() {
	}
}

func (c *Controller) processNextItem() bool {
	key, quit := c.queue.Get()
	if quit {
		return false
	}
	defer c.queue.Done(key)

	err := c.updateSecrets(key.(string))
	c.handleErr(err, key)
	return true
}

func (c *Controller) handleErr(err error, key interface{}) {
	if err == nil {
		c.queue.Forget(key)
		return
	}

	if c.queue.NumRequeues(key) < 5 {
		log.Printf("error syncing secret %v: %v", key, err)

		c.queue.AddRateLimited(key)
		return
	}

	c.queue.Forget(key)
	runtime.HandleError(err)
	log.Printf("dropping pod %q out of the queue: %v", key, err)
}
