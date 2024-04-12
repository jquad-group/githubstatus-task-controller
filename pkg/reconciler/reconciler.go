/*
Copyright 2021 The Tekton Authors

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

package reconciler

import (
	"context"
	"errors"
	"fmt"
	"strings"

	githubStatusClient "github.com/jquad-group/githubstatus-task-controller/pkg/git"
	"github.com/tektoncd/pipeline/pkg/apis/pipeline/v1beta1"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/clock"
	"knative.dev/pkg/apis"
	"knative.dev/pkg/controller"
	"knative.dev/pkg/logging"
	kreconciler "knative.dev/pkg/reconciler"
)

const (
	WaitTaskCancelledMsg          string = "Wait Task cancelled."
	WaitTaskFailedOnRunTimeoutMsg string = "Wait Task failed as it times out."
)

// Reconciler implements controller.Reconciler for Configuration resources.
type Reconciler struct {
	Clock clock.PassiveClock
}

// ReconcileKind implements Interface.ReconcileKind.
func (c *Reconciler) ReconcileKind(ctx context.Context, r *v1beta1.CustomRun) kreconciler.Event {
	logger := logging.FromContext(ctx)
	logger.Infof("Reconciling %s/%s", r.Namespace, r.Name)

	if r.Spec.CustomRef == nil ||
		r.Spec.CustomRef.APIVersion != "pipeline.jquad.rocks/v1alpha1" || r.Spec.CustomRef.Kind != "GithubStatus" {
		// This is not a CustomRun we should have been notified about; do nothing.
		return nil
	}

	if r.Spec.CustomRef.Name != "" {
		logger.Errorf("Found unexpected ref name: %s", r.Spec.CustomRef.Name)
		r.Status.MarkCustomRunFailed("UnexpectedName", "Found unexpected ref name: %s", r.Spec.CustomRef.Name)
		return nil
	}

	errParams := Validate(r)
	if errParams != nil {
		logger.Error(errParams.Error())
		r.Status.MarkCustomRunFailed("MissingParameter", errParams.Error())
		return nil
	}

	if len(r.Spec.Params) != 9 {
		var found []string
		for _, p := range r.Spec.Params {
			if p.Name == "revision" || p.Name == "baseUrl" || p.Name == "owner" || p.Name == "repository" || p.Name == "state" || p.Name == "description" || p.Name == "context" || p.Name == "targetUrl" || p.Name == "insecureSkipVerify" {
				continue
			}
			found = append(found, p.Name)
		}
		logger.Errorf("Found unexpected params: %v", found)
		r.Status.MarkCustomRunFailed("UnexpectedParams", "Found unexpected params: %v", found)
		return nil
	}

	baseUrl := r.Spec.GetParam("baseUrl").Value.StringVal
	owner := r.Spec.GetParam("owner").Value.StringVal
	repository := r.Spec.GetParam("repository").Value.StringVal
	revision := r.Spec.GetParam("revision").Value.StringVal
	state := r.Spec.GetParam("state").Value.StringVal
	description := r.Spec.GetParam("description").Value.StringVal
	context := r.Spec.GetParam("context").Value.StringVal
	targetUrl := r.Spec.GetParam("targetUrl").Value.StringVal
	insecureSkipVerify := r.Spec.GetParam("insecureSkipVerify").Value.StringVal

	timeout := r.GetTimeout()

	if !r.HasStarted() {
		logger.Info("CustomRun hasn't started, start it")
		r.Status.InitializeConditions()
		r.Status.StartTime = &metav1.Time{Time: c.Clock.Now()}
		r.Status.MarkCustomRunRunning("Running", "Waiting for duration to elapse")
	}

	// Ignore completed waits.
	if r.IsDone() {
		logger.Info("CustomRun is finished, done reconciling")
		return nil
	}

	// Skip if the CustomRun is cancelled.
	if r.IsCancelled() {
		logger.Infof("The CustomRun %v has been cancelled", r.GetName())
		r.Status.CompletionTime = &metav1.Time{Time: c.Clock.Now()}
		var msg string = fmt.Sprint(r.Spec.StatusMessage)
		if msg == "" {
			msg = WaitTaskCancelledMsg
		}
		logger.Infof(msg)
		r.Status.MarkCustomRunFailed("Cancelled", msg)
		return nil
	}

	elapsed := c.Clock.Since(r.Status.StartTime.Time)

	// Custom Task Timed Out
	if r.Status.StartTime != nil && elapsed > timeout {
		logger.Infof("The CustomRun %v timed out", r.GetName())
		// Retry if the current RetriesStatus hasn't reached the retries limit
		if r.Spec.Retries > len(r.Status.RetriesStatus) {
			logger.Infof("CustomRun timed out, retrying... %#v", r.Status)
			retryCustomRun(r, apis.Condition{
				Type:   apis.ConditionSucceeded,
				Status: v1.ConditionFalse,
				Reason: "TimedOut",
			})
			return controller.NewRequeueImmediately()
		}

		r.Status.CompletionTime = &metav1.Time{Time: c.Clock.Now()}
		r.Status.MarkCustomRunFailed("TimedOut", WaitTaskFailedOnRunTimeoutMsg)

		return nil
	}
	statusClient := githubStatusClient.NewGithubClient(baseUrl, owner, repository, revision, "blabla", stringToBool(insecureSkipVerify))
	err, _ := statusClient.SetStatus(state, description, context, targetUrl)
	if err != nil {
		r.Status.CompletionTime = &metav1.Time{Time: c.Clock.Now()}
		r.Status.MarkCustomRunFailed("Failed", err.Error())
		return nil
	}
	logger.Infof("The github status is set")
	r.Status.MarkCustomRunSucceeded("GithubStatusSet", "The github status was set")

	// Don't emit events on nop-reconciliations, it causes scale problems.
	return nil
}

func retryCustomRun(r *v1beta1.CustomRun, c apis.Condition) {
	// Add retry history
	newStatus := r.Status.DeepCopy()
	newStatus.RetriesStatus = nil
	condSet := r.GetConditionSet()
	condSet.Manage(newStatus).SetCondition(c)
	r.Status.RetriesStatus = append(r.Status.RetriesStatus, *newStatus)

	// Clear status
	r.Status.StartTime = nil

	r.Status.MarkCustomRunRunning("Retrying", "")
}

func Validate(r *v1beta1.CustomRun) error {
	exprBaseUrl := r.Spec.GetParam("baseUrl")
	if exprBaseUrl == nil || exprBaseUrl.Value.StringVal == "" {
		return errors.New("The baseUrl param was not passed")
	}

	exprOwner := r.Spec.GetParam("owner")
	if exprOwner == nil || exprOwner.Value.StringVal == "" {
		return errors.New("The owner param was not passed")
	}

	exprRepository := r.Spec.GetParam("repository")
	if exprRepository == nil || exprRepository.Value.StringVal == "" {
		return errors.New("The repository param was not passed")
	}

	exprRevision := r.Spec.GetParam("revision")
	if exprRevision == nil || exprRevision.Value.StringVal == "" {
		return errors.New("The revision param was not passed")
	}

	exprGithubState := r.Spec.GetParam("state")
	if exprGithubState == nil || exprGithubState.Value.StringVal == "" {
		return errors.New("The state param was not passed")
	}

	exprGithubStatusDescription := r.Spec.GetParam("description")
	if exprGithubStatusDescription == nil || exprGithubStatusDescription.Value.StringVal == "" {
		return errors.New("The description param was not passed")
	}

	exprGithubStatusContext := r.Spec.GetParam("context")
	if exprGithubStatusContext == nil || exprGithubStatusContext.Value.StringVal == "" {
		return errors.New("The context param was not passed")
	}

	exprGithubStatusTargetUrl := r.Spec.GetParam("targetUrl")
	if exprGithubStatusTargetUrl == nil || exprGithubStatusTargetUrl.Value.StringVal == "" {
		return errors.New("The targetUrl param was not passed")
	}

	exprGithubInsecureSkipVerify := r.Spec.GetParam("insecureSkipVerify")
	if exprGithubInsecureSkipVerify == nil || exprGithubInsecureSkipVerify.Value.StringVal == "" {
		return errors.New("The insecureSkipVerify param was not passed")
	}

	return nil
}

func stringToBool(s string) bool {
	switch strings.ToLower(s) {
	case "true":
		return true
	case "false":
		return false
	default:
		return false
	}
}
