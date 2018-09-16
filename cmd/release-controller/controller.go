package main

import (
	"fmt"
	"time"

	"github.com/golang/glog"
	lru "github.com/hashicorp/golang-lru"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	batchinformers "k8s.io/client-go/informers/batch/v1"
	batchclient "k8s.io/client-go/kubernetes/typed/batch/v1"
	kv1core "k8s.io/client-go/kubernetes/typed/core/v1"
	batchlisters "k8s.io/client-go/listers/batch/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/record"
	"k8s.io/client-go/util/workqueue"

	imagev1 "github.com/openshift/api/image/v1"
	imagescheme "github.com/openshift/client-go/image/clientset/versioned/scheme"
	imageclient "github.com/openshift/client-go/image/clientset/versioned/typed/image/v1"
	imageinformers "github.com/openshift/client-go/image/informers/externalversions/image/v1"
	imagelisters "github.com/openshift/client-go/image/listers/image/v1"
)

// Controller ensures that OpenShift update payload images (also known as
// release images) are created whenever an image stream representing the images
// in a release is updated. A consumer sets the release.openshift.io/config
// annotation on an image stream in the release namespace and the controller will
//
// 1. Create a tag in the "release" image stream that uses the release name +
//    current timestamp.
// 2. Mirror all of the tags in the input image stream so that they can't be
//    pruned.
// 3. Launch a job in the job namespace to invoke 'oc adm release new' from
//    the mirror pointing to the release tag we created in step 1.
// 4. If the job succeeds in pushing the tag, set an annotation on that tag
//    release.openshift.io/phase = "Ready", indicating that the release can be
//    used by other steps
//
// TODO:
//
// 5. Perform a number of manual and automated tasks on the release - if all are
//    successful, set the phase to "Verified" and then promote the tag to external
//    locations.
//
// Invariants:
//
// 1. ...
//
type Controller struct {
	eventRecorder record.EventRecorder

	imageClient       imageclient.ImageV1Interface
	imageStreamLister *multiImageStreamLister

	jobClient batchclient.JobsGetter
	jobLister batchlisters.JobLister

	// syncs are the items that must return true before the queue can be processed
	syncs []cache.InformerSynced

	// queue is the list of namespace keys that must be synced.
	queue workqueue.RateLimitingInterface

	// expectations track upcoming changes that we have not yet observed
	expectations *expectations
	// expectationDelay controls how long the controller waits to observe its
	// own creates. Exposed only for testing.
	expectationDelay time.Duration

	releaseNamespace string
	jobNamespace     string

	sourceCache *lru.Cache
}

// NewController instantiates a Controller
func NewController(
	eventsClient kv1core.EventsGetter,
	imageClient imageclient.ImageV1Interface,
	jobClient batchclient.JobsGetter,
	jobs batchinformers.JobInformer,
	releaseNamespace string,
	jobNamespace string,
) *Controller {
	broadcaster := record.NewBroadcaster()
	broadcaster.StartLogging(glog.V(2).Infof)
	// TODO: remove the wrapper when every clients have moved to use the clientset.
	broadcaster.StartRecordingToSink(&kv1core.EventSinkImpl{Interface: eventsClient.Events("")})
	recorder := broadcaster.NewRecorder(imagescheme.Scheme, corev1.EventSource{Component: "release-controller"})

	sourceCache, err := lru.New(50)
	if err != nil {
		panic(err)
	}

	c := &Controller{
		eventRecorder: recorder,
		queue:         workqueue.NewNamedRateLimitingQueue(workqueue.DefaultControllerRateLimiter(), releaseImageStreamName),

		expectations:     newExpectations(),
		expectationDelay: 2 * time.Second,

		imageClient:       imageClient,
		imageStreamLister: &multiImageStreamLister{listers: make(map[string]imagelisters.ImageStreamNamespaceLister)},

		jobClient: jobClient,
		jobLister: jobs.Lister(),

		syncs: []cache.InformerSynced{},

		releaseNamespace: releaseNamespace,
		jobNamespace:     jobNamespace,

		sourceCache: sourceCache,
	}

	// any change to a job
	jobs.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    c.processJobIfComplete,
		DeleteFunc: c.processJob,
		UpdateFunc: func(oldObj, newObj interface{}) { c.processJobIfComplete(newObj) },
	})

	return c
}

// multiImageStreamLister uses multiple independent namespace listers
// to simulate a full lister so that multiple namespaces can be watched
// for image streams.
type multiImageStreamLister struct {
	listers map[string]imagelisters.ImageStreamNamespaceLister
}

func (l *multiImageStreamLister) List(label labels.Selector) ([]*imagev1.ImageStream, error) {
	var streams []*imagev1.ImageStream
	for _, ns := range l.listers {
		is, err := ns.List(label)
		if err != nil {
			return nil, err
		}
		streams = append(streams, is...)
	}
	return streams, nil
}

func (l *multiImageStreamLister) ImageStreams(ns string) imagelisters.ImageStreamNamespaceLister {
	return l.listers[ns]
}

func (c *Controller) AddNamespacedImageStreamInformer(ns string, imagestreams imageinformers.ImageStreamInformer) {
	c.imageStreamLister.listers[ns] = imagestreams.Lister().ImageStreams(ns)

	c.syncs = append(c.syncs, imagestreams.Informer().HasSynced)
	imagestreams.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    c.processImageStream,
		DeleteFunc: c.processImageStream,
		UpdateFunc: func(oldObj, newObj interface{}) {
			c.processImageStream(newObj)
		},
	})
}

type queueKey struct {
	namespace string
	name      string
}

func (c *Controller) processNamespace(obj interface{}) {
	switch t := obj.(type) {
	case metav1.Object:
		ns := t.GetNamespace()
		if len(ns) == 0 {
			utilruntime.HandleError(fmt.Errorf("object %T has no namespace", obj))
			return
		}
		c.queue.Add(queueKey{namespace: ns})
	default:
		utilruntime.HandleError(fmt.Errorf("couldn't get key for object %T", obj))
	}
}

func (c *Controller) processJob(obj interface{}) {
	switch t := obj.(type) {
	case *batchv1.Job:
		key, ok := queueKeyFor(t.Annotations["release.openshift.io/source"])
		if !ok {
			return
		}
		if glog.V(4) {
			success, complete := jobIsComplete(t)
			glog.Infof("Job %s updated, complete=%t success=%t", t.Name, complete, success)
		}
		c.queue.Add(key)
	default:
		utilruntime.HandleError(fmt.Errorf("couldn't get key for object %T", obj))
	}
}

func (c *Controller) processJobIfComplete(obj interface{}) {
	switch t := obj.(type) {
	case *batchv1.Job:
		if _, complete := jobIsComplete(t); !complete {
			return
		}
		c.processJob(obj)
	default:
		utilruntime.HandleError(fmt.Errorf("couldn't get key for object %T", obj))
	}
}

func (c *Controller) processImageStream(obj interface{}) {
	switch t := obj.(type) {
	case *imagev1.ImageStream:
		// when we see a change to an image stream, reset our expectations
		// this also allows periodic purging of the expectation list in the event
		// we miss one or more events.
		c.expectations.Clear(t.Namespace, t.Name)

		// if this image stream is a mirror for releases, requeue any that it touches
		if _, ok := t.Annotations["release.openshift.io/config"]; ok {
			glog.V(5).Infof("Image stream %s is a release input and will be queued", t.Name)
			c.queue.Add(queueKey{namespace: t.Namespace, name: t.Name})
			return
		}
		if key, ok := queueKeyFor(t.Annotations["release.openshift.io/source"]); ok {
			glog.V(5).Infof("Image stream %s was created by %v, queuing source", t.Name, key)
			c.queue.Add(key)
			return
		}
		if t.Namespace == c.releaseNamespace && t.Name == releaseImageStreamName {
			// if the release image stream is modified, just requeue everything in the event a tag
			// has been deleted
			glog.V(5).Infof("Image stream %s is a release target, requeue both namespaces", t.Name)
			c.queue.Add(queueKey{namespace: c.releaseNamespace})
			c.queue.Add(queueKey{namespace: c.jobNamespace})
			return
		}
	default:
		utilruntime.HandleError(fmt.Errorf("couldn't get key for object %T", obj))
	}
}

// Run begins watching and syncing.
func (c *Controller) Run(workers int, stopCh <-chan struct{}) {
	defer utilruntime.HandleCrash()
	defer c.queue.ShutDown()

	glog.Infof("Starting controller")

	if !cache.WaitForCacheSync(stopCh, c.syncs...) {
		utilruntime.HandleError(fmt.Errorf("timed out waiting for caches to sync"))
		return
	}

	for i := 0; i < workers; i++ {
		go wait.Until(c.worker, time.Second, stopCh)
	}

	<-stopCh
	glog.Infof("Shutting down controller")
}

func (c *Controller) worker() {
	for c.processNext() {
	}
	glog.V(4).Infof("Worker stopped")
}

func (c *Controller) processNext() bool {
	key, quit := c.queue.Get()
	if quit {
		return false
	}
	defer c.queue.Done(key)

	glog.V(5).Infof("processing %v begin", key)
	err := c.sync(key.(queueKey))
	c.handleNamespaceErr(err, key)
	glog.V(5).Infof("processing %v end", key)

	return true
}

type terminalError struct {
	error
}

func (c *Controller) handleNamespaceErr(err error, key interface{}) {
	if err == nil {
		c.queue.Forget(key)
		return
	}

	if _, ok := err.(terminalError); ok {
		glog.V(2).Infof("Unable to sync %v, no retry: %v", key, err)
		return
	}

	glog.V(2).Infof("Error syncing %v: %v", key, err)
	c.queue.AddRateLimited(key)
}