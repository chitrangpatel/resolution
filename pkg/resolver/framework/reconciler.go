/*
Copyright 2022 The Tekton Authors

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

package framework

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/tektoncd/resolution/pkg/apis/resolution/v1alpha1"
	rrclient "github.com/tektoncd/resolution/pkg/client/clientset/versioned"
	rrv1alpha1 "github.com/tektoncd/resolution/pkg/client/listers/resolution/v1alpha1"
	resolutioncommon "github.com/tektoncd/resolution/pkg/common"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"
	"knative.dev/pkg/controller"
	"knative.dev/pkg/logging"
	"knative.dev/pkg/reconciler"
)

type Reconciler struct {
	// Implements reconciler.LeaderAware
	reconciler.LeaderAwareFuncs

	resolver                 Resolver
	kubeClientSet            kubernetes.Interface
	resourceRequestLister    rrv1alpha1.ResourceRequestLister
	resourceRequestClientSet rrclient.Interface
}

var _ reconciler.LeaderAware = &Reconciler{}

// TODO(sbwsg): This should be configurable via ConfigMap. It differs
// from the ResourceRequest reconciler's timeout mostly for testing at this
// point.
const defaultMaximumResolutionDuration = 30 * time.Second

func (r *Reconciler) Reconcile(ctx context.Context, key string) error {
	namespace, name, err := cache.SplitMetaNamespaceKey(key)
	if err != nil {
		err = &resolutioncommon.ErrorInvalidResourceKey{Key: key, Original: err}
		return controller.NewPermanentError(err)
	}

	rr, err := r.resourceRequestLister.ResourceRequests(namespace).Get(name)
	if err != nil {
		err := &resolutioncommon.ErrorGettingResource{ResolverName: "resourcerequest", Key: key, Original: err}
		return controller.NewPermanentError(err)
	}

	if rr.IsDone() {
		return nil
	}

	ctx, cancelFn := context.WithTimeout(ctx, defaultMaximumResolutionDuration)
	defer cancelFn()
	return r.resolve(ctx, key, rr)
}

func (r *Reconciler) resolve(ctx context.Context, key string, rr *v1alpha1.ResourceRequest) error {
	errChan := make(chan error, 0)
	resourceChan := make(chan ResolvedResource, 0)

	go func() {
		validationError := r.resolver.ValidateParams(ctx, rr.Spec.Parameters)
		if validationError != nil {
			errChan <- &resolutioncommon.ErrorInvalidRequest{
				ResourceRequestKey: key,
				Message:            validationError.Error(),
			}
			return
		}
		resource, resolveErr := r.resolver.Resolve(ctx, rr.Spec.Parameters)
		if resolveErr != nil {
			errChan <- &resolutioncommon.ErrorGettingResource{
				ResolverName: r.resolver.GetName(ctx),
				Key:          key,
				Original:     resolveErr,
			}
			return
		}
		resourceChan <- resource
	}()

	select {
	case err := <-errChan:
		if err != nil {
			return r.OnError(ctx, rr, err)
		}
	case <-ctx.Done():
		if err := ctx.Err(); err != nil {
			return r.OnError(ctx, rr, err)
		}
	case resource := <-resourceChan:
		return r.writeResolvedData(ctx, rr, resource)
	}

	return errors.New("unknown error")
}

// OnError is used to handle any situation where a ResourceRequest has
// reached a terminal situation that cannot be recovered from.
func (r *Reconciler) OnError(ctx context.Context, rr *v1alpha1.ResourceRequest, err error) error {
	if rr == nil {
		return controller.NewPermanentError(err)
	}
	if err != nil {
		_ = r.MarkFailed(ctx, rr, err)
		return controller.NewPermanentError(err)
	}
	return nil
}

// MarkFailed updates a ResourceRequest as having failed. It returns
// errors that occur during the update process or nil if the update
// appeared to succeed.
func (r *Reconciler) MarkFailed(ctx context.Context, rr *v1alpha1.ResourceRequest, resolutionErr error) error {
	key := fmt.Sprintf("%s/%s", rr.Namespace, rr.Name)
	reason, resolutionErr := resolutioncommon.ReasonError(resolutionErr)
	latestGeneration, err := r.resourceRequestClientSet.ResolutionV1alpha1().ResourceRequests(rr.Namespace).Get(ctx, rr.Name, metav1.GetOptions{})
	if err != nil {
		logging.FromContext(ctx).Warnf("error getting latest generation of resourcerequest %q: %v", key, err)
		return err
	}
	if latestGeneration.IsDone() {
		return nil
	}
	latestGeneration.Status.MarkFailed(reason, resolutionErr.Error())
	_, err = r.resourceRequestClientSet.ResolutionV1alpha1().ResourceRequests(rr.Namespace).UpdateStatus(ctx, latestGeneration, metav1.UpdateOptions{})
	if err != nil {
		logging.FromContext(ctx).Warnf("error marking resourcerequest %q as failed: %v", key, err)
		return err
	}
	return nil
}

// statusDataPatch is the json structure that will be PATCHed into
// a ResourceRequest with its data and annotations once successfully
// resolved.
type statusDataPatch struct {
	Annotations map[string]string `json:"annotations"`
	Data        string            `json:"data"`
}

func (r *Reconciler) writeResolvedData(ctx context.Context, rr *v1alpha1.ResourceRequest, resource ResolvedResource) error {
	encodedData := base64.StdEncoding.Strict().EncodeToString(resource.Data())
	patchBytes, err := json.Marshal(map[string]statusDataPatch{
		"status": statusDataPatch{
			Data:        encodedData,
			Annotations: resource.Annotations(),
		},
	})
	if err != nil {
		return r.OnError(ctx, rr, &resolutioncommon.ErrorUpdatingRequest{
			ResourceRequestKey: fmt.Sprintf("%s/%s", rr.Namespace, rr.Name),
			Original:           fmt.Errorf("error serializing resource request patch: %w", err),
		})
	}
	_, err = r.resourceRequestClientSet.ResolutionV1alpha1().ResourceRequests(rr.Namespace).Patch(ctx, rr.Name, types.MergePatchType, patchBytes, metav1.PatchOptions{}, "status")
	if err != nil {
		return r.OnError(ctx, rr, &resolutioncommon.ErrorUpdatingRequest{
			ResourceRequestKey: fmt.Sprintf("%s/%s", rr.Namespace, rr.Name),
			Original:           err,
		})
	}

	return nil
}
