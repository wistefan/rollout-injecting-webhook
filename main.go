package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/argoproj/argo-rollouts/pkg/apis/rollouts/v1alpha1"
	"github.com/gookit/goutil/structs"
	"github.com/procyon-projects/chrono"
	"github.com/rs/zerolog/log"
	admission "k8s.io/api/admission/v1"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/serializer"
	"k8s.io/apimachinery/pkg/types"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	dynamic "k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"

	"encoding/json"
)

var (
	runtimeScheme = runtime.NewScheme()
	codecFactory  = serializer.NewCodecFactory(runtimeScheme)
	deserializer  = codecFactory.UniversalDeserializer()
	taskScheduler = chrono.NewDefaultTaskScheduler()
)

type MetaData struct {
	Name            string            `json:"name"`
	Labels          map[string]string `json:"labels"`
	Annotations     map[string]string `json:"annotations"`
	ResourceVersion string            `json:"resourceVersion,omitempty"`
}

type Selector struct {
	MatchLabels map[string]string `json:"matchLabels"`
}

type WorkloadRef struct {
	ApiVersion string `json:"apiVersion"`
	Kind       string `json:"kind"`
	Name       string `json:"name"`
}

type BlueGreenStrategy struct {
	ActiveService        string `json:"activeService"`
	PreviewService       string `json:"previewService"`
	PreviewReplicaCount  int32  `json:"previewReplicaCount"`
	AutoPromotionEnabled bool   `json:"autoPromotionEnabled"`
}

type Strategy struct {
	BlueGreen BlueGreenStrategy `json:"blueGreen"`
}

type RolloutSpec struct {
	Replicas    int32       `json:"replicas"`
	Selector    Selector    `json:"selector"`
	WorkloadRef WorkloadRef `json:"workloadRef"`
	Strategy    Strategy    `json:"strategy"`
}

type FixedRollout struct {
	ApiVersion string      `json:"apiVersion"`
	Kind       string      `json:"kind"`
	MetaData   MetaData    `json:"metadata"`
	Spec       RolloutSpec `json:"spec"`
}

// add kind AdmissionReview in scheme
func init() {
	_ = corev1.AddToScheme(runtimeScheme)
	_ = admission.AddToScheme(runtimeScheme)
	_ = appsv1.AddToScheme(runtimeScheme)
}

type admitv1Func func(admission.AdmissionReview) *admission.AdmissionResponse

type admitHandler struct {
	v1 admitv1Func
}

func AdmitHandler(f admitv1Func) admitHandler {
	return admitHandler{
		v1: f,
	}
}

// serve handles the http portion of a request prior to handing to an admit
// function
func serve(w http.ResponseWriter, r *http.Request, admit admitHandler) {
	var body []byte
	if r.Body != nil {
		if data, err := io.ReadAll(r.Body); err == nil {
			body = data
		}
	}

	// verify the content type is accurate
	contentType := r.Header.Get("Content-Type")
	if contentType != "application/json" {
		log.Error().Msgf("contentType=%s, expect application/json", contentType)
		return
	}

	log.Info().Msgf("handling request: %s", body)
	var responseObj runtime.Object
	if obj, gvk, err := deserializer.Decode(body, nil, nil); err != nil {
		msg := fmt.Sprintf("Request could not be decoded: %v", err)
		log.Error().Msg(msg)
		http.Error(w, msg, http.StatusBadRequest)
		return

	} else {
		requestedAdmissionReview, ok := obj.(*admission.AdmissionReview)
		if !ok {
			log.Error().Msgf("Expected v1.AdmissionReview but got: %T", obj)
			return
		}
		responseAdmissionReview := &admission.AdmissionReview{}
		responseAdmissionReview.SetGroupVersionKind(*gvk)
		responseAdmissionReview.Response = admit.v1(*requestedAdmissionReview)
		responseAdmissionReview.Response.UID = requestedAdmissionReview.Request.UID
		responseObj = responseAdmissionReview

	}
	log.Info().Msgf("sending response: %v", responseObj)
	respBytes, err := json.Marshal(responseObj)
	if err != nil {
		log.Err(err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	if _, err := w.Write(respBytes); err != nil {
		log.Err(err)
	}
}

func serveMutate(w http.ResponseWriter, r *http.Request) {
	serve(w, r, AdmitHandler(mutate))
}
func serveValidate(w http.ResponseWriter, r *http.Request) {
	serve(w, r, AdmitHandler(validate))
}

func mutate(ar admission.AdmissionReview) (response *admission.AdmissionResponse) {
	defer func() {
		// recover from panic if one occurred. Set err to nil otherwise.
		if recover() != nil {
			log.Warn().Msg("Something bad happend. Dont block the cluster!")
			response = &admission.AdmissionResponse{Allowed: true}
		}
	}()

	log.Info().Msgf("mutating deployments")
	deploymentResource := metav1.GroupVersionResource{Group: "apps", Version: "v1", Resource: "deployments"}
	if ar.Request.Resource != deploymentResource {
		log.Error().Msgf("expect resource to be %s", deploymentResource)
		return nil
	}
	raw := ar.Request.Object.Raw
	deployment := appsv1.Deployment{}

	if _, _, err := deserializer.Decode(raw, nil, &deployment); err != nil {
		log.Err(err)
		return &admission.AdmissionResponse{
			Result: &metav1.Status{
				Message: err.Error(),
			},
		}
	}

	val, ok := deployment.Annotations["wistefan/rollout-injecting-webhook"]
	if ok && val == "ignore" {
		log.Info().Msgf("Rollout injection is disabled for %s.", deployment.Name)
		return &admission.AdmissionResponse{Allowed: true}
	}
	clientSet, err := getClientSet()
	if err != nil {
		// no mutations if the client fails to not block the cluster
		return &admission.AdmissionResponse{Allowed: true}
	}
	err = handleRolloutCreation(deployment, clientSet)
	if err != nil {
		scheduleRolloutCreation(deployment, clientSet, 10)
		return &admission.AdmissionResponse{Allowed: true}
	}

	deploymentPatch := `[{ "op": "replace", "path": "/spec/replicas", "value": 0 }, {"op": "add", "path": "/metadata/annotations/wistefan~1rollout-injecting-webhook", "value":"managed"}]`
	pt := admission.PatchTypeJSONPatch
	return &admission.AdmissionResponse{Allowed: true, PatchType: &pt, Patch: []byte(deploymentPatch)}
}

func handleRolloutCreation(deployment appsv1.Deployment, clientSet *kubernetes.Clientset) error {

	val, ok := deployment.Annotations["wistefan/rollout-injecting-webhook"]
	if ok && val == "managed" {
		log.Debug().Msgf("Rollout already created for deployment %s.", deployment.Name)
		return nil
	}

	activeService, previewService, _ := createPreviewService(clientSet, deployment)
	if activeService == "" {
		log.Info().Msgf("No service found for %s. Will allow deployment and reschedule the rollout creation.", deployment.Name)
		return errors.New("no_active_service_found")

	}
	log.Info().Msgf("Services to use: %s and %s.", activeService, previewService)
	_, err := createRollout(clientSet, deployment, activeService, previewService)
	if err != nil {
		log.Err(err).Msg("Was not able to create rollout.")
	}
	return err
}

func scheduleRolloutCreation(deployment appsv1.Deployment, clientSet *kubernetes.Clientset, backoffSeconds int) error {
	now := time.Now()
	_, err := taskScheduler.Schedule(func(ctx context.Context) {
		err := handleRolloutCreation(deployment, clientSet)
		if err == nil {
			err = updateDeployment(deployment, clientSet)
		}
		if err != nil {
			log.Warn().Msgf("Was not able to update. Will reschedule the task. Err: %s", err)
			// if the rollout cannot be created after such long time, it most probably will also not later.
			if backoffSeconds > 60 {
				log.Warn().Msgf("Backoff limit reached. Will not reschedule the rollout creation.")
			} else {
				scheduleRolloutCreation(deployment, clientSet, backoffSeconds+10)
			}
		}
	}, chrono.WithStartTime(now.Year(), now.Month(), now.Day(), now.Hour(), now.Minute(), now.Second()+backoffSeconds))

	return err
}

func updateDeployment(deployment appsv1.Deployment, clientSet *kubernetes.Clientset) error {
	ctx := context.TODO()
	deploymentClient := clientSet.AppsV1().Deployments(deployment.Namespace)

	deploymentPatch := `[{ "op": "replace", "path": "/spec/replicas", "value": 0 }, {"op": "add", "path": "/metadata/annotations/wistefan~1rollout-injecting-webhook", "value":"managed"}]`

	_, err := deploymentClient.Patch(ctx, deployment.Name, types.JSONPatchType, []byte(deploymentPatch), metav1.PatchOptions{})

	return err
}

func getClientSet() (clientset *kubernetes.Clientset, err error) {
	// creates the in-cluster config
	cfg, err := rest.InClusterConfig()
	if err != nil {
		log.Err(err).Msg("Was not able to create an in-cluster config.")
		return
	}
	// creates the client
	clientset, err = kubernetes.NewForConfig(cfg)
	if err != nil {
		log.Err(err).Msg("Was not able to create a clientset.")
		return

	}
	return
}

func createRollout(clientSet *kubernetes.Clientset, deployment appsv1.Deployment, activeService string, previewService string) (rolloutName string, err error) {
	ctx := context.TODO()
	cfg, _ := rest.InClusterConfig()

	dynamicClient, _ := dynamic.NewForConfig(cfg)

	var previewReplicas int32 = 1
	var autoPromotion bool = false

	metaData := MetaData{Name: deployment.Name, Annotations: map[string]string{"argocd.argoproj.io/compare-options": "IgnoreExtraneous", "argocd.argoproj.io/sync-options": "Prune=false"}, Labels: deployment.Labels}

	rolloutSpec := RolloutSpec{
		Replicas:    *deployment.Spec.Replicas,
		Selector:    Selector{MatchLabels: deployment.Spec.Selector.MatchLabels},
		WorkloadRef: WorkloadRef{ApiVersion: deployment.APIVersion, Kind: deployment.Kind, Name: deployment.Name},
		Strategy:    Strategy{BlueGreen: BlueGreenStrategy{ActiveService: activeService, PreviewService: previewService, PreviewReplicaCount: previewReplicas, AutoPromotionEnabled: autoPromotion}},
	}
	rollout := FixedRollout{
		ApiVersion: "argoproj.io/v1alpha1",
		Kind:       "Rollout",
		MetaData:   metaData,
		Spec:       rolloutSpec,
	}

	json, _ := json.Marshal(rollout)
	log.Info().Msgf("Will create rollout: %s", json)

	un := unstructured.Unstructured{}
	contentMap, _ := structs.StructToMap(rollout)
	un.SetUnstructuredContent(contentMap)
	un.SetOwnerReferences(deployment.OwnerReferences)
	oldUnstructured, _ := dynamicClient.Resource(v1alpha1.RolloutGVR).Namespace(deployment.Namespace).Get(ctx, deployment.Name, metav1.GetOptions{})
	if oldUnstructured == nil {
		_, err = dynamicClient.Resource(v1alpha1.RolloutGVR).Namespace(deployment.Namespace).Create(ctx, &un, metav1.CreateOptions{})
	} else {
		un.SetResourceVersion(oldUnstructured.GetResourceVersion())
		_, err = dynamicClient.Resource(v1alpha1.RolloutGVR).Namespace(deployment.Namespace).Update(ctx, &un, metav1.UpdateOptions{})
	}

	if err != nil {
		log.Err(err).Msg("Was not able to create the rollout.")
	}
	return deployment.Name, err
}

func createPreviewService(clientSet *kubernetes.Clientset, deployment appsv1.Deployment) (activeServiceName, previewServiceName string, err error) {
	serviceClient := clientSet.CoreV1().Services(deployment.Namespace)

	ctx := context.TODO()
	services, err := serviceClient.List(ctx, metav1.ListOptions{})
	if err != nil {
		log.Err(err).Msg("Was not able to retrieve services.")
	}

	deploymentSelectors := deployment.Spec.Selector.MatchLabels
	log.Debug().Msgf("Deployment selectors: %s", deploymentSelectors)
	var originalService corev1.Service
	serviceExists := false
	for _, service := range services.Items {
		log.Debug().Msgf("Service selectors: %s", service.Spec.Selector)
		if containsSelectors(deploymentSelectors, service.Spec.Selector) {
			originalService = service
			serviceExists = true
			break
		}
	}
	if !serviceExists {
		log.Info().Msgf("No service exists for deployment %s. Rollout injection is not supported.", deployment.Name)
		return
	}

	previewServiceAnnotations := originalService.Annotations
	previewServiceAnnotations["argocd.argoproj.io/compare-options"] = "IgnoreExtraneous"
	previewServiceAnnotations["argocd.argoproj.io/sync-options"] = "Prune=false"

	previewSpec := corev1.ServiceSpec{Ports: originalService.Spec.Ports, Type: originalService.Spec.Type}
	previewServiceMetaData := metav1.ObjectMeta{Name: originalService.ObjectMeta.Name + "-preview", Labels: originalService.Labels, Annotations: previewServiceAnnotations}
	previewService := corev1.Service{Spec: previewSpec, ObjectMeta: previewServiceMetaData}

	for _, service := range services.Items {
		if service.Name == previewService.Name {
			// service already exists
			return originalService.Name, previewService.Name, err
		}
	}
	_, err = serviceClient.Create(ctx, &previewService, metav1.CreateOptions{})
	if err != nil {
		log.Err(err).Msg("Was not able to create the service.")
		return
	}
	return originalService.Name, previewService.Name, err
}

func containsSelectors(contained map[string]string, containingMap map[string]string) bool {
	for containedKey, containedValue := range contained {
		log.Info().Msgf("Contained key %s and value %s - check in map %s", containedKey, containedValue, containingMap)
		val, ok := containingMap[containedKey]
		if !ok {
			return false
		}
		if containedValue != val {
			return false
		}
	}
	return true
}

// verify if a Deployment has the 'prod' prefix name
func validate(ar admission.AdmissionReview) *admission.AdmissionResponse {
	log.Info().Msgf("validating deployments")

	return &admission.AdmissionResponse{Allowed: true}
}

func main() {
	var tlsKey, tlsCert string
	flag.StringVar(&tlsKey, "tlsKey", "/etc/certs/tls.key", "Path to the TLS key")
	flag.StringVar(&tlsCert, "tlsCert", "/etc/certs/tls.crt", "Path to the TLS certificate")
	flag.Parse()
	http.HandleFunc("/mutate", serveMutate)
	http.HandleFunc("/validate", serveValidate)
	log.Info().Msg("Server started ...")
	log.Fatal().Err(http.ListenAndServeTLS(":8443", tlsCert, tlsKey, nil)).Msg("webhook server exited")
}
