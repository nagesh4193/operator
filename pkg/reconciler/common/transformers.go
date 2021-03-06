/*
Copyright 2020 The Tekton Authors

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package common

import (
	"context"
	"log"
	"os"
	"strings"

	mf "github.com/manifestival/manifestival"
	"github.com/tektoncd/operator/pkg/apis/operator/v1alpha1"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"knative.dev/pkg/logging"
)

const (
	AnnotationPreserveNS = "operator.tekton.dev/preserve-namespace"
	PipelinesImagePrefix = "IMAGE_PIPELINES_"
	TriggersImagePrefix  = "IMAGE_TRIGGERS_"
	AddonsImagePrefix    = "IMAGE_ADDONS_"

	ArgPrefix   = "arg_"
	ParamPrefix = "param_"
)

// transformers that are common to all components.
func transformers(ctx context.Context, obj v1alpha1.TektonComponent) []mf.Transformer {
	return []mf.Transformer{
		mf.InjectOwner(obj),
		injectNamespaceConditional(AnnotationPreserveNS, obj.GetSpec().GetTargetNamespace()),
		injectNamespaceCRDWebhookClientConfig(obj.GetSpec().GetTargetNamespace()),
	}
}

// Transform will mutate the passed-by-reference manifest with one
// transformed by platform, common, and any extra passed in
func Transform(ctx context.Context, manifest *mf.Manifest, instance v1alpha1.TektonComponent, extra ...mf.Transformer) error {
	logger := logging.FromContext(ctx)
	logger.Debug("Transforming manifest")

	transformers := transformers(ctx, instance)
	transformers = append(transformers, extra...)

	m, err := manifest.Transform(transformers...)
	if err != nil {
		instance.GetStatus().MarkInstallFailed(err.Error())
		return err
	}
	*manifest = m
	return nil
}

func injectNamespaceConditional(preserveNamespace, targetNamespace string) mf.Transformer {
	tf := mf.InjectNamespace(targetNamespace)
	return func(u *unstructured.Unstructured) error {
		annotations := u.GetAnnotations()
		val, ok := annotations[preserveNamespace]
		if ok && val == "true" {
			return nil
		}
		return tf(u)
	}
}

func injectNamespaceCRDWebhookClientConfig(targetNamespace string) mf.Transformer {
	return func(u *unstructured.Unstructured) error {
		kind := strings.ToLower(u.GetKind())
		if kind != "customresourcedefinition" {
			return nil
		}
		service, found, err := unstructured.NestedFieldNoCopy(u.Object, "spec", "conversion", "webhookClientConfig", "service")
		if !found || err != nil {
			return err
		}
		m := service.(map[string]interface{})
		if _, ok := m["namespace"]; ok {
			m["namespace"] = targetNamespace
		}
		return nil
	}
}

// ImagesFromEnv will provide map of key value.
func ImagesFromEnv(prefix string) map[string]string {
	images := map[string]string{}
	for _, env := range os.Environ() {
		if !strings.HasPrefix(env, prefix) {
			continue
		}

		keyValue := strings.Split(env, "=")
		name := strings.TrimPrefix(keyValue[0], prefix)
		url := keyValue[1]
		images[name] = url
	}

	return images
}

// ToLowerCaseKeys converts key value to lower cases.
func ToLowerCaseKeys(keyValues map[string]string) map[string]string {
	newMap := map[string]string{}

	for k, v := range keyValues {
		key := strings.ToLower(k)
		newMap[key] = v
	}

	return newMap
}

// DeploymentImages replaces container and args images.
func DeploymentImages(images map[string]string) mf.Transformer {
	return func(u *unstructured.Unstructured) error {
		if u.GetKind() != "Deployment" {
			return nil
		}

		d := &appsv1.Deployment{}
		err := runtime.DefaultUnstructuredConverter.FromUnstructured(u.Object, d)
		if err != nil {
			return err
		}

		containers := d.Spec.Template.Spec.Containers
		replaceContainerImages(containers, images)

		unstrObj, err := runtime.DefaultUnstructuredConverter.ToUnstructured(d)
		if err != nil {
			return err
		}
		u.SetUnstructuredContent(unstrObj)

		return nil
	}
}

func replaceContainerImages(containers []corev1.Container, images map[string]string) {
	for i, container := range containers {
		name := formKey("", container.Name)
		if url, exist := images[name]; exist {
			containers[i].Image = url
		}

		replaceContainersArgsImage(&container, images)
	}
}

func replaceContainersArgsImage(container *corev1.Container, images map[string]string) {
	for a, arg := range container.Args {
		if argVal, hasArg := splitsByEqual(arg); hasArg {
			argument := formKey(ArgPrefix, argVal[0])
			if url, exist := images[argument]; exist {
				container.Args[a] = argVal[0] + "=" + url
			}
			continue
		}

		argument := formKey(ArgPrefix, arg)
		if url, exist := images[argument]; exist {
			container.Args[a+1] = url
		}
	}

}

func formKey(prefix, arg string) string {
	argument := strings.ToLower(arg)
	if prefix != "" {
		argument = prefix + argument
	}
	return strings.ReplaceAll(argument, "-", "_")
}

func splitsByEqual(arg string) ([]string, bool) {
	values := strings.Split(arg, "=")
	if len(values) == 2 {
		return values, true
	}

	return values, false
}

// TaskImages replaces step and params images.
func TaskImages(images map[string]string) mf.Transformer {
	return func(u *unstructured.Unstructured) error {
		if u.GetKind() != "ClusterTask" {
			return nil
		}

		steps, found, err := unstructured.NestedSlice(u.Object, "spec", "steps")
		if err != nil {
			return err
		}
		if !found {
			return nil
		}
		replaceStepsImages(steps, images)
		err = unstructured.SetNestedField(u.Object, steps, "spec", "steps")
		if err != nil {
			return err
		}

		params, found, err := unstructured.NestedSlice(u.Object, "spec", "params")
		if err != nil {
			return err
		}
		if !found {
			return nil
		}
		replaceParamsImage(params, images)
		err = unstructured.SetNestedField(u.Object, params, "spec", "params")
		if err != nil {
			return err
		}
		return nil
	}
}

func replaceStepsImages(steps []interface{}, override map[string]string) {
	for _, s := range steps {
		step := s.(map[string]interface{})
		name, ok := step["name"].(string)
		if !ok {
			log.Println("Unable to get the step", "step", s)
			continue
		}

		name = formKey("", name)
		image, found := override[name]
		if !found || image == "" {
			log.Println("Image not found", "step", name, "action", "skip")
			continue
		}
		step["image"] = image
	}
}

func replaceParamsImage(params []interface{}, override map[string]string) {
	for _, p := range params {
		param := p.(map[string]interface{})
		name, ok := param["name"].(string)
		if !ok {
			log.Println("Unable to get the pram", "param", p)
			continue
		}

		name = formKey(ParamPrefix, name)
		image, found := override[name]
		if !found || image == "" {
			log.Println("Image not found", "step", name, "action", "skip")
			continue
		}
		param["default"] = image
	}
}
