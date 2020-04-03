// Copyright 2018 Tetrate Labs
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package main

import (
	"context"
	"io/ioutil"
	"log"
	"os"
	"strings"
	"time"

	"github.com/pkg/errors"
	"github.com/spf13/cobra"
	"github.com/tetratelabs/istio-cloud-map/pkg/serviceentry"
	ic "istio.io/client-go/pkg/clientset/versioned"
	icinformer "istio.io/client-go/pkg/informers/externalversions/networking/v1alpha3"
	"k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/uuid"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/tetratelabs/istio-cloud-map/pkg/cloudmap"
	"github.com/tetratelabs/istio-cloud-map/pkg/control"
)

const (
	apiGroup      = "networking.istio.io"
	apiVersion    = "v1alpha3"
	apiType       = apiGroup + "/" + apiVersion
	kind          = "ServiceEntry"
	allNamespaces = ""
	resyncPeriod  = 30
)

func serve() (serve *cobra.Command) {
	var (
		id         string
		debug      bool
		kubeConfig string
		namespace  string
		awsRegion  string
		awsID      string
		awsSecret  string
	)

	serve = &cobra.Command{
		Use:     "serve",
		Aliases: []string{"serve"},
		Short:   "Starts the Istio Cloud Map Operator server",
		Example: "istio-cloud-map serve --id 123",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := clientcmd.BuildConfigFromFlags("", kubeConfig)
			if err != nil {
				return errors.Wrapf(err, "failed to create a kube client from the config %q", kubeConfig)
			}
			ic, err := ic.NewForConfig(cfg)
			if err != nil {
				return errors.Wrap(err, "failed to create an istio client from the k8s rest config")
			}

			t := true
			sessionUUID := uuid.NewUUID()
			owner := v1.OwnerReference{
				APIVersion: "cloudmap.istio.io",
				Kind:       "ServiceController",
				Name:       id,
				Controller: &t,
				UID:        sessionUUID,
			}

			// TODO: move over to run groups, get a context there to use to handle shutdown gracefully.
			ctx := context.Background() // common context for cancellation across all loops/routines

			// TODO: see if it makes sense to push this down into the CM section after moving to run groups
			if len(awsRegion) == 0 {
				if region, set := os.LookupEnv("AWS_REGION"); set {
					awsRegion = region
				}
			}

			cloudMap := cloudmap.NewStore()
			log.Printf("Starting Cloud Map watcher in %q", awsRegion)
			cmWatcher, err := cloudmap.NewWatcher(cloudMap, awsRegion, awsID, awsSecret)
			if err != nil {
				return err
			}
			go cmWatcher.Run(ctx)

			istio := serviceentry.New(owner)
			if debug {
				istio = serviceentry.NewLoggingStore(istio, log.Printf)
			}
			log.Print("Starting Synchronizer control loop")

			// we get the service entry for namespace `namespace` for the synchronizer to publish service entries in to
			// (if we use an `allNamespaces` client here we can't publish). Listening for ServiceEntries is done with
			// the informer, which uses allNamespace.
			write := ic.NetworkingV1alpha3().ServiceEntries(findNamespace(namespace))
			sync := control.NewSynchronizer(owner, istio, cloudMap, write)
			go sync.Run(ctx)

			informer := icinformer.NewServiceEntryInformer(ic, allNamespaces, 5*time.Second,
				// taken from https://github.com/istio/istio/blob/release-1.5/pilot/pkg/bootstrap/namespacecontroller.go
				cache.Indexers{cache.NamespaceIndex: cache.MetaNamespaceIndexFunc})
			serviceentry.AttachHandler(istio, informer)
			log.Printf("Watching %s.%s across all namespaces with resync period %d and id %q", apiType, kind, resyncPeriod, id)
			informer.Run(ctx.Done())
			return nil
		},
	}

	serve.PersistentFlags().StringVar(&id,
		"id", "istio-cloud-map-operator", "ID of this instance; instances will only ServiceEntries marked with their own ID.")
	serve.PersistentFlags().BoolVar(&debug, "debug", true, "if true, enables more logging")
	serve.PersistentFlags().StringVar(&kubeConfig,
		"kube-config", "", "kubeconfig location; if empty the server will assume it's in a cluster; for local testing use ~/.kube/config")
	serve.PersistentFlags().StringVar(&namespace, "namespace", "",
		"If provided, the namespace this operator publishes CRDs to. If no value is provided it will be populated from the WATCH_NAMESPACE environment variable.")

	serve.PersistentFlags().StringVar(&awsRegion, "aws-region", "", "AWS Region to connect to Cloud Map in")
	serve.PersistentFlags().StringVar(&awsID, "aws-access-key-id", "",
		"AWS Key ID to use to connect to Cloud Map. Use flags for both this and aws-secret OR use "+
			"the environment variables AWS_ACCESS_KEY_ID and AWS_SECRET_ACCESS_KEY. Flags and env vars cannot be mixed.")
	serve.PersistentFlags().StringVar(&awsSecret, "aws-secret-access-key", "",
		"AWS Key ID to use to connect to Cloud Map. Use flags for both this and aws-secret OR use "+
			"the environment variables AWS_ACCESS_KEY_ID and AWS_SECRET_ACCESS_KEY. Flags and env vars cannot be mixed.")
	return serve
}

func main() {
	root := &cobra.Command{
		Short:   "istio-cloud-map",
		Example: "",
	}
	// TODO: add other commands for listing services under management, etc.
	root.AddCommand(serve())
	if err := root.Execute(); err != nil {
		log.Fatal(err)
	}
}

func findNamespace(namespace string) string {
	if len(namespace) > 0 {
		log.Printf("using namespace flag to publish service entries into %q", namespace)
		return namespace
	}
	// This way assumes you've set the POD_NAMESPACE environment variable using the downward API.
	// This check has to be done first for backwards compatibility with the way InClusterConfig was originally set up
	if ns, ok := os.LookupEnv("POD_NAMESPACE"); ok {
		log.Printf("using POD_NAMESPACE environment variable to publish service entries into %q", namespace)
		return ns
	}

	// Fall back to the namespace associated with the service account token, if available
	if data, err := ioutil.ReadFile("/var/run/secrets/kubernetes.io/serviceaccount/namespace"); err == nil {
		if ns := strings.TrimSpace(string(data)); len(ns) > 0 {
			log.Printf("using service account namespace from pod filesystem to publish service entries into %q", namespace)
			return ns
		}
	}

	log.Printf("couldn't determine a namespace, falling back to %q", "default")
	return "default"
}
