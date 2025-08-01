package main

import (
	"context"
	"flag"
	"fmt"
	"net/http"
	"os"

	"github.com/psrvere/k8s-controllers/job-handler/controllers"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
)

var (
	scheme   = runtime.NewScheme()
	setupLog = ctrl.Log.WithName("setup")
)

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
}

func main() {
	var probeAddr string
	flag.String("health-probe-bind-address", ":8080", "Probe endpoint binds to this address")

	opts := zap.Options{
		Development: true,
	}

	opts.BindFlags(flag.CommandLine)
	flag.Parse()
	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&opts)))

	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		Scheme:                 scheme,
		HealthProbeBindAddress: probeAddr,
	})
	if err != nil {
		setupLog.Error(err, "Unable to start manager")
		os.Exit(1)
	}

	if err = (&controllers.JobHandlerReconciler{
		Client: mgr.GetClient(),
		Scheme: mgr.GetScheme(),
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "JobHandler")
		os.Exit(1)
	}

	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		setupLog.Error(err, "unable to setup health check")
		os.Exit(1)
	}

	// Custom readiness check that verifies the controller can access Kubernetes resources
	if err := mgr.AddReadyzCheck("readyz", func(req *http.Request) error {
		// Check if we can list jobs (basic connectivity test)
		jobList := &batchv1.JobList{}
		if err := mgr.GetClient().List(context.Background(), jobList, &client.ListOptions{Limit: 1}); err != nil {
			return fmt.Errorf("failed to list jobs: %w", err)
		}

		// Check if we can list pods (required for log collection)
		podList := &corev1.PodList{}
		if err := mgr.GetClient().List(context.Background(), podList, &client.ListOptions{Limit: 1}); err != nil {
			return fmt.Errorf("failed to list pods: %w", err)
		}

		// Check if we can list configmaps (required for storing results)
		configMapList := &corev1.ConfigMapList{}
		if err := mgr.GetClient().List(context.Background(), configMapList, &client.ListOptions{Limit: 1}); err != nil {
			return fmt.Errorf("failed to list configmaps: %w", err)
		}

		return nil
	}); err != nil {
		setupLog.Error(err, "unable to setup ready check")
		os.Exit(1)
	}

	setupLog.Info("starting manager")
	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		setupLog.Error(err, "problem running manager")
		os.Exit(1)
	}
}
