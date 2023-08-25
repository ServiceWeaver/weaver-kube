// Copyright 2023 Google LLC
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

package impl

import (
	_ "embed"
	"fmt"
	"os"
	"time"

	"github.com/ServiceWeaver/weaver/runtime/protos"
	appsv1 "k8s.io/api/apps/v1"
	_ "k8s.io/api/autoscaling/v2beta2"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"sigs.k8s.io/yaml"
)

// TODO(rgrandl): We might want to revisit the way we integrate with external
// systems to export observability information. For example, we might want to
// create an agent abstraction that is used by the babysitters to export
// observability info. Different implementations of these agents will behave
// differently. For example, a Jaeger agent, will simply export traces to a Jaeger
// service. A Prometheus agent will export a /metrics endpoint that can be scraped
// by a Prometheus service. Another agent might want to convert otel traces to
// a different format and export it (e.g., Elastic). This way, we can add agent
// implementations for any observability systems.

const (
	// The names of the observability services that interact with the application.
	tracesConfigKey  = "jaeger_service"
	metricsConfigKey = "prometheus_service"
	logsConfigKey    = "loki_service"
	grafanaConfigKey = "grafana_service"

	// Jaeger related configs.

	// Name of the Jaeger application.
	jaegerAppName = "jaeger"

	// Name of the Jaeger [1] container image used for automatically started Jaeger service.
	//
	// all-in-one[1] combines the three Jaeger components: agent, collector, and
	// query service/UI in a single binary, which is enough for handling the traces
	// in a kubernetes deployment. However, we don't really need an agent. Also,
	// we may want to launch separate collector and query services later on. Or,
	// we may want to launch an otel collector service as well, to ensure that the
	// traces are available, even if the deployment is deleted.
	//
	// [1] https://www.jaegertracing.io/docs/1.45/deployment/#all-in-one
	autoJaegerImageName = "jaegertracing/all-in-one"

	// The port on which the Jaeger UI agent is listening on.
	//
	// Note that this is expected to be the default port [1] for both the automatically
	// started Jaeger UI agent and the one started by the user.
	//
	// [1] https://www.jaegertracing.io/docs/1.6/getting-started/
	defaultJaegerUIPort = 16686

	// The port on which the Jaeger collector is receiving traces from the
	// clients when using the Jaeger exporter.
	//
	// Note that this is expected to be the default port [1] for both the automatically
	// started Jaeger collector and the one started by the user.
	//
	// [1] https://www.jaegertracing.io/docs/1.6/getting-started/
	defaultJaegerCollectorPort = 14268

	// Prometheus related configs.

	// Name of the Prometheus [1] container image used for automatically started Prometheus service.
	//
	// [1] https://prometheus.io/
	autoPrometheusImageName = "prom/prometheus:v2.30.3"

	// The port on which the weavelets are exporting the metrics.
	//
	// Note that this is expected to be the default port [1] for both the automatically
	// started Prometheus and the one started by the user.
	//
	// [1] https://opensource.com/article/18/12/introduction-prometheus
	defaultMetricsPort = 9090

	// Loki related configs.

	// Name of the Loki [1] container image used for automatically started Loki service.
	//
	// [1] https://grafana.com/oss/loki/
	autoLokiImageName = "grafana/loki"

	// The port on which Loki is exporting the logs.
	//
	// Note that this is expected to be the default port [1] for both the automatically
	// started Loki and the one started by the user.
	//
	// [1] https://grafana.com/docs/loki/latest/configuration/
	defaultLokiPort = 3100

	// Promtail related configs.

	// Name of the Promtail [1] container image used for automatically started Promtail.
	//
	// [1] https://grafana.com/docs/loki/latest/clients/promtail/
	autoPromtailImageName = "grafana/promtail"

	// Grafana related configs.

	// Name of the Grafana [1] container image used for automatically started Grafana.
	//
	// [1] https://grafana.com/
	autoGrafanaImageName = "grafana/grafana"

	// The default Grafana web server port.
	//
	// Note that this is expected to be the default port [1] for both the automatically
	// started Grafana and the one started by the user.
	//
	// [1] https://grafana.com/docs/grafana/latest/setup-grafana/configure-grafana/
	defaultGrafanaPort = 3000

	// The below values are used to check the values passed through app config
	// for different observability services.

	// Set iff the kube deployer should generate Kubernetes configs to deploy
	// a service.
	auto = ""

	// Set iff the user requires that the corresponding service should be disabled
	// entirely. I.e., neither the user started the service nor the Kube deployer.
	disabled = "none"
)

// dashboard was generated using the Grafana UI. Then, we saved the content as
// a JSON file.
//
//go:embed dashboard.txt
var dashboardContent string

// generateObservabilityConfigs generates Kubernetes configurations for exporting
// applications' metrics, logs, and traces.
func generateObservabilityConfigs(dep *protos.Deployment, cfg *KubeConfig) ([]byte, error) {
	configs, err := generateConfigsToExportTraces(dep, cfg)
	if err != nil {
		return nil, fmt.Errorf("unable to create kube configs to export traces: %w", err)
	}
	var generated []byte
	generated = append(generated, configs...)

	configs, err = generateConfigsToExportMetrics(dep, cfg)
	if err != nil {
		return nil, fmt.Errorf("unable to create kube configs to export metrics: %w", err)
	}
	generated = append(generated, configs...)

	configs, err = generateConfigsToExportLogs(dep, cfg)
	if err != nil {
		return nil, fmt.Errorf("unable to create kube configs to export logs: %w", err)
	}
	generated = append(generated, configs...)

	// Generate Kubernetes configs to export logs, traces and metrics to Grafana.
	configs, err = generateConfigsToExportToGrafana(dep, cfg)
	if err != nil {
		return nil, fmt.Errorf("unable to create kube configs to export data to Grafana: %w", err)
	}
	generated = append(generated, configs...)
	return generated, nil
}

// generateConfigsToExportTraces generates Jaeger Kubernetes deployment
// configurations for a given app.
//
// Note that the configs should be generated iff the kube deployer automatically
// runs a Jaeger service along with the app.
//
// Note that we run a single instance of Jaeger. This is because we are using
// a Jaeger image that combines three Jaeger components, agent, collector, and
// query service/UI in a single image.
//
// TODO(rgrandl): If the trace volume can't be handled by a single instance, we
// should scale these components independently, and use different image(s).
//
// TODO(rgrandl): Convert the below comments into docs.
// How to integrate with an external Jaeger service?
// E.g., if you use Helm [1] to install Jaeger, you can simply do the following:
// 1) You install Jaeger using a command similar to the one below.
// helm install jaeger-all-in-one jaeger-all-in-one/jaeger-all-in-one
//
// 2) Your Jaeger service has the name 'jaeger-all-in-one'.
//
// 3) in your config.toml, set the 'jaeger_service' info as follows:
// config.toml
// ...
// [kube]
// observability = {jaeger_service = "jaeger-all-in-one"}
//
// [1] https://helm.sh/
func generateConfigsToExportTraces(dep *protos.Deployment, cfg *KubeConfig) ([]byte, error) {
	// The user disabled exporting the traces, don't generate anything.
	if cfg.Observability[tracesConfigKey] != auto {
		return nil, nil
	}

	jname := name{dep.App.Name, jaegerAppName}.DNSLabel()

	// Generate the Jaeger deployment.
	d := &appsv1.Deployment{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "apps/v1",
			Kind:       "Deployment",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      jname,
			Namespace: cfg.Namespace,
		},
		Spec: appsv1.DeploymentSpec{
			Replicas: ptrOf(int32(1)),
			Selector: &metav1.LabelSelector{
				MatchLabels: map[string]string{
					"jaeger": jname,
				},
			},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						"jaeger": jname,
					},
					Namespace: cfg.Namespace,
				},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{
							Name:            jname,
							Image:           fmt.Sprintf("%s:latest", autoJaegerImageName),
							ImagePullPolicy: corev1.PullIfNotPresent,
						},
					},
				},
			},
			Strategy: appsv1.DeploymentStrategy{
				Type:          "RollingUpdate",
				RollingUpdate: &appsv1.RollingUpdateDeployment{},
			},
		},
	}
	content, err := yaml.Marshal(d)
	if err != nil {
		return nil, err
	}
	var generated []byte
	generated = append(generated, []byte("# Jaeger Deployment\n")...)
	generated = append(generated, content...)
	generated = append(generated, []byte("\n---\n")...)
	fmt.Fprintf(os.Stderr, "Generated Jaeger deployment\n")

	// Generate the Jaeger service.
	s := &corev1.Service{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "v1",
			Kind:       "Service",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      jname,
			Namespace: cfg.Namespace,
		},
		Spec: corev1.ServiceSpec{
			Selector: map[string]string{"jaeger": jname},
			Ports: []corev1.ServicePort{
				{
					Name:       "ui-port",
					Port:       defaultJaegerUIPort,
					Protocol:   "TCP",
					TargetPort: intstr.IntOrString{IntVal: int32(defaultJaegerUIPort)},
				},
				{
					Name:       "collector-port",
					Port:       defaultJaegerCollectorPort,
					Protocol:   "TCP",
					TargetPort: intstr.IntOrString{IntVal: int32(defaultJaegerCollectorPort)},
				},
			},
		},
	}
	content, err = yaml.Marshal(s)
	if err != nil {
		return nil, err
	}
	generated = append(generated, []byte("\n# Jaeger Service\n")...)
	generated = append(generated, content...)
	generated = append(generated, []byte("\n---\n")...)
	fmt.Fprintf(os.Stderr, "Generated Jaeger service\n")

	return generated, nil
}

// generateConfigsToExportMetrics generates the Prometheus kubernetes deployment
// information for a given app.
//
// TODO(rgrandl): Convert the below comments into docs.
// How to integrate with an external Prometheus service?
// E.g., if you use Helm [1] to install Prometheus, you can simply do the following:
// 1) You install Prometheus using a command similar to the one below.
// helm install prometheus prometheus-community/prometheus
//
// 2) Your Prometheus service has the name 'prometheus-server'.
//
// 3) Write a simple manifest file prom.yaml that contains the scrape config info
// generated by the kube deployer. The scrape config info should look something like:
//
//	```
//	 - job_name: "collatz-prometheus-ca03bb5f"
//	   metrics_path: /metrics
//	 ...
//
//	 You can find the Kube generated config map by running kubectl cm -n <namespace>.
//	 It should look something like collatz-prometheus-config-xyz.
//
// 4) The prom.yaml file should look like:
//
//	extraScrapeConfigs: |
//	 - job_name: "collatz-prometheus-ca03bb5f"
//	   metrics_path: /metrics
//	...
//
// 5) Upgrade your prometheus release with the new manifest file.
// helm upgrade prometheus prometheus-community/prometheus -f prom.yaml
//
// 6) Now you should be able to see the app traces with your running Prometheus service.
//
// Note that this will work disregarding whether you disabled the kube deployer
// to generate a Prometheus service as well.
//
// [Optional] However, if you run a Grafana service, and the service is generated
// by the Kube deployer, then if you specify the name of your prometheus service
// in the config.toml file, the Grafana service will automatically import your
// Prometheus service as a datasource.
//
// config.toml
// ...
// [kube]
// observability = {prometheus_service = "prometheus-server"}
//
// [1] https://helm.sh/
func generateConfigsToExportMetrics(dep *protos.Deployment, cfg *KubeConfig) ([]byte, error) {
	// The user disabled exporting the metrics, don't generate anything.
	if cfg.Observability[metricsConfigKey] == disabled {
		return nil, nil
	}

	// Generate configs to configure Prometheus to scrape metrics from the app.
	var generated []byte
	content, err := generatePrometheusConfigs(dep, cfg)
	if err != nil {
		return nil, err
	}
	generated = append(generated, content...)

	// Generate the Prometheus kubernetes deployment info iff the kube deployer
	// should automatically start the Prometheus service.
	if cfg.Observability[metricsConfigKey] != auto {
		return generated, nil
	}

	// Generate kubernetes service configs for Prometheus.
	content, err = generatePrometheusServiceConfigs(dep, cfg)
	if err != nil {
		return nil, err
	}
	generated = append(generated, content...)

	return generated, nil
}

// generatePrometheusConfigs generate configs needed by the Prometheus service
// to export metrics.
//
// Note that these configs are needed by both the automatically started Prometheus
// service and the one started by the user.
func generatePrometheusConfigs(dep *protos.Deployment, cfg *KubeConfig) ([]byte, error) {
	cname := name{dep.App.Name, "prometheus", "config"}.DNSLabel()
	pname := name{dep.App.Name, "prometheus"}.DNSLabel()

	// Build the config map that holds the prometheus configuration file. In the
	// config we specify how to scrape the app pods for the metrics.
	//
	// Note that the info in the config map will be used by the Prometheus service
	// deployed along the app by the kube deployer, or by the user if they run their
	// own instance of Prometheus.
	config := fmt.Sprintf(`
global:
  scrape_interval: 15s
scrape_configs:
  - job_name: "%s"
    metrics_path: %s
    kubernetes_sd_configs:
      - role: pod
        namespaces:
          names:
            - %s
    scheme: http
    relabel_configs:
      - source_labels: [__meta_kubernetes_pod_label_metrics]
        regex: "%s"
        action: keep
`, pname, prometheusEndpoint, cfg.Namespace, dep.App.Name)

	// Create a config map to store the prometheus config.
	cm := corev1.ConfigMap{
		TypeMeta: metav1.TypeMeta{
			Kind:       "ConfigMap",
			APIVersion: "v1",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      cname,
			Namespace: cfg.Namespace,
		},
		Data: map[string]string{
			"prometheus.yaml": config,
		},
	}
	content, err := yaml.Marshal(cm)
	if err != nil {
		return nil, err
	}
	var generated []byte
	generated = append(generated, []byte(fmt.Sprintf("\n# Config Map %s\n", cname))...)
	generated = append(generated, content...)
	generated = append(generated, []byte("\n---\n")...)
	fmt.Fprintf(os.Stderr, "Generated kube deployment for config map %s\n", cname)

	return generated, nil
}

// generatePrometheusServiceConfigs generates the Prometheus kubernetes
// service information for a given app.
//
// Note that the configs should be generated iff the kube deployer automatically
// runs a Prometheus service along with the app.
//
// TODO(rgrandl): We run a single instance of Prometheus for now. We might want
// to scale it up if it becomes a bottleneck.
func generatePrometheusServiceConfigs(dep *protos.Deployment, cfg *KubeConfig) ([]byte, error) {
	cname := name{dep.App.Name, "prometheus", "config"}.DNSLabel()
	pname := name{dep.App.Name, "prometheus"}.DNSLabel()

	// Build the kubernetes Prometheus deployment.
	d := &appsv1.Deployment{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "apps/v1",
			Kind:       "Deployment",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      pname,
			Namespace: cfg.Namespace,
		},
		Spec: appsv1.DeploymentSpec{
			Replicas: ptrOf(int32(1)),
			Selector: &metav1.LabelSelector{
				MatchLabels: map[string]string{"prometheus": pname},
			},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels:    map[string]string{"prometheus": pname},
					Namespace: cfg.Namespace,
				},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{
							Name:            pname,
							Image:           autoPrometheusImageName,
							ImagePullPolicy: corev1.PullIfNotPresent,
							Args: []string{
								fmt.Sprintf("--config.file=/etc/%s/prometheus.yaml", pname),
								fmt.Sprintf("--storage.tsdb.path=/%s", pname),
							},
							Ports: []corev1.ContainerPort{{ContainerPort: defaultMetricsPort}},
							VolumeMounts: []corev1.VolumeMount{
								{
									Name:      cname,
									MountPath: fmt.Sprintf("/etc/%s/prometheus.yaml", pname),
									SubPath:   "prometheus.yaml",
								},
								{
									Name:      fmt.Sprintf("%s-data", pname),
									MountPath: fmt.Sprintf("/%s", pname),
								},
							},
						},
					},
					Volumes: []corev1.Volume{
						{
							Name: cname,
							VolumeSource: corev1.VolumeSource{
								ConfigMap: &corev1.ConfigMapVolumeSource{
									LocalObjectReference: corev1.LocalObjectReference{
										Name: cname,
									},
									Items: []corev1.KeyToPath{
										{
											Key:  "prometheus.yaml",
											Path: "prometheus.yaml",
										},
									},
								},
							},
						},
						{
							Name: fmt.Sprintf("%s-data", pname),
						},
					},
				},
			},
			Strategy: appsv1.DeploymentStrategy{
				Type:          "RollingUpdate",
				RollingUpdate: &appsv1.RollingUpdateDeployment{},
			},
		},
	}
	content, err := yaml.Marshal(d)
	if err != nil {
		return nil, err
	}

	var generated []byte
	generated = append(generated, []byte(fmt.Sprintf("\n# Prometheus Deployment %s\n", pname))...)
	generated = append(generated, content...)
	generated = append(generated, []byte("\n---\n")...)
	fmt.Fprintf(os.Stderr, "Generated kube deployment for Prometheus %s\n", pname)

	// Build the kubernetes Prometheus service.
	s := &corev1.Service{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "v1",
			Kind:       "Service",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      pname,
			Namespace: cfg.Namespace,
		},
		Spec: corev1.ServiceSpec{
			Selector: map[string]string{"prometheus": pname},
			Ports: []corev1.ServicePort{
				{
					Port:       servicePort,
					Protocol:   "TCP",
					TargetPort: intstr.IntOrString{IntVal: int32(defaultMetricsPort)},
				},
			},
		},
	}
	content, err = yaml.Marshal(s)
	if err != nil {
		return nil, err
	}
	generated = append(generated, []byte(fmt.Sprintf("\n# Prometheus Service %s\n", pname))...)
	generated = append(generated, content...)
	generated = append(generated, []byte("\n---\n")...)
	fmt.Fprintf(os.Stderr, "Generated kube service for Prometheus %s\n", pname)

	return generated, nil
}

// generateConfigsToExportLogs generates the Loki/Promtail kubernetes deployment
// information for a given app.
//
// Note that for the Loki to be able to aggregate logs, we need to run Promtail
// on each node where the app is deployed.
//
// TODO(rgrandl): Convert the below comments into docs.
// How to integrate with an external Loki service?
// E.g., if you use Helm [1] to install Loki, you can simply do the following:
// 1) You install Loki using a command similar to the one below.
// helm install loki grafana/loki-stack
//
// 2) You install Promtail using a command similar to the one below.
// helm install promtail grafana/promtail
//
// Assume that your Loki service name is 'loki' and the Promtail daemonset name
// is 'promtail'.
//
// 3) You don't need to update the 'loki' service at all.
//
// 4) You have to ugrade the 'promtail' daemonset with the content of the config
// generated by the kube deployer. The kube generated config can be found by running
// kubectl cm -n <namespace>. It should look something like collatz-promtail-config-xyz:
//
//	clients:
//	 - url: http://loki:3100/loki/api/v1/push
//	...
//	scrape_configs:
//	- job_name: kubernetes-pods
//
// 5) Write a simple manifest file promtail.yaml that contains the `clients` and the
// `scrape_config` sections from the config.
//
// 6) The promtail.yaml file should look like:
//
//	clients:
//	- url: http://loki:3100/loki/api/v1/push
//	extraScrapeConfigs: |
//	- job_name: kubernetes-pods
//	...
//
// 7) Upgrade your Promtail release with the new manifest file.
// helm upgrade promtail grafana/promtail -f promtail.yaml
//
// 8) In your config.toml file, you should set the name of the loki service as follows:
//
//	config.toml
//	...
//	[kube]
//	observability = {loki_service = "loki"}
//
//	This is optional. However, if you set it, then the kube generated config
//	for promtail will set the right path to the Loki service for you, so you
//	can easily upgrade your Promtail release. Also, if you launch Grafana,
//	it will automatically add your Loki service as a datasource.
//
// [1] https://helm.sh/
func generateConfigsToExportLogs(dep *protos.Deployment, cfg *KubeConfig) ([]byte, error) {
	// The user disabled exporting the logs, don't generate anything.
	if cfg.Observability[logsConfigKey] == disabled {
		return nil, nil
	}

	// Generate configs to configure Loki/Promtail.
	var generated []byte
	content, err := generateLokiConfigs(dep, cfg)
	if err != nil {
		return nil, err
	}
	generated = append(generated, content...)

	content, err = generatePromtailConfigs(dep, cfg)
	if err != nil {
		return nil, err
	}
	generated = append(generated, content...)

	// Generate the Loki/Promtail kubernetes deployment configs iff the kube deployer
	// should deploy the Loki/Promtail.
	if cfg.Observability[logsConfigKey] != auto {
		return generated, nil
	}

	// Generate kubernetes service configs for Loki/Promtail.
	content, err = generateLokiServiceConfigs(dep, cfg)
	if err != nil {
		return nil, err
	}
	generated = append(generated, content...)

	content, err = generatePromtailAgentConfigs(dep, cfg)
	if err != nil {
		return nil, err
	}
	generated = append(generated, content...)

	return generated, nil
}

// generateLokiConfigs generate configs needed by a Loki service to
// aggregate app logs.
//
// Note that these configs are needed by both the automatically started Loki
// service and the one started by the user.
//
// TODO(rgrandl): check if we can simplify the configurations.
func generateLokiConfigs(dep *protos.Deployment, cfg *KubeConfig) ([]byte, error) {
	cname := name{dep.App.Name, "loki", "config"}.DNSLabel()
	lname := name{dep.App.Name, "loki"}.DNSLabel()

	timeSchemaEnabledFromFn := func() string {
		current := time.Now()
		year, month, day := current.Date()
		return fmt.Sprintf("%d-%02d-%02d", year, month, day)
	}

	// Build the config map that holds the Loki configuration file. In the
	// config we specify how to store the logs and the schema. Right now we have
	// a very simple in-memory store [1].
	//
	// TODO(rgrandl): There are millions of knobs to tune the config. We might revisit
	// this in the future.
	//
	// [1] https://grafana.com/docs/loki/latest/operations/storage/boltdb-shipper/
	config := fmt.Sprintf(`
auth_enabled: false
server:
  http_listen_port: %d

common:
  instance_addr: 127.0.0.1
  path_prefix: /tmp/%s
  storage:
    filesystem:
      chunks_directory: /tmp/%s/chunks
      rules_directory: /tmp/%s/rules
  replication_factor: 1
  ring:
    kvstore:
      store: inmemory

schema_config:
  configs:
    - from: %s  # Marks the starting point of this schema
      store: boltdb-shipper
      object_store: filesystem
      schema: v11
      index:
        prefix: index_
        period: 24h
`, defaultLokiPort, lname, lname, lname, timeSchemaEnabledFromFn())

	// Create a config map to store the Loki config.
	cm := corev1.ConfigMap{
		TypeMeta: metav1.TypeMeta{
			Kind:       "ConfigMap",
			APIVersion: "v1",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      cname,
			Namespace: cfg.Namespace,
		},
		Data: map[string]string{
			"loki.yaml": config,
		},
	}
	content, err := yaml.Marshal(cm)
	if err != nil {
		return nil, err
	}

	var generated []byte
	generated = append(generated, []byte(fmt.Sprintf("\n# Config Map %s\n", cname))...)
	generated = append(generated, content...)
	generated = append(generated, []byte("\n---\n")...)
	fmt.Fprintf(os.Stderr, "Generated kube deployment for config map %s\n", cname)

	return generated, nil
}

// generatePromtailConfigs generates configuration needed to enable Promtail
// to retrieve the app logs.
//
// Note that these configs are needed by both the automatically started Promtail
// agent and the one started by the user.
//
// TODO(rgrandl): check if we can simplify the configurations.
func generatePromtailConfigs(dep *protos.Deployment, cfg *KubeConfig) ([]byte, error) {
	promName := name{dep.App.Name, "promtail"}.DNSLabel()

	var lokiURL string
	lservice := cfg.Observability[logsConfigKey]
	switch {
	case lservice == auto:
		// lokiURL should point to the Loki service generated by Kube.
		lokiURL = name{dep.App.Name, "loki"}.DNSLabel()
	case lservice != disabled:
		// lokiURL should point to the Loki service provided by the user.
		lokiURL = lservice
	default:
		// No Loki service URL to set.
	}

	// This configuration is a simplified version of the Promtail config generated
	// by helm [1]. Right now we scrape only logs from the pods. We may want to
	// scrape system information and nodes info as well.
	//
	// The scraped logs are sent to Loki for indexing and being stored.
	//
	// [1] https://helm.sh/docs/topics/charts/.
	config := fmt.Sprintf(`
server:
  log_format: logfmt
  http_listen_port: 3101

clients:
  - url: http://%s:%d/loki/api/v1/push

positions:
  filename: /run/promtail/positions.yaml

scrape_configs:
  - job_name: kubernetes-pods
    kubernetes_sd_configs:
      - role: pod
        namespaces:
          names:
            - %s
    relabel_configs:
      - source_labels:
          - __meta_kubernetes_pod_label_appName
        regex: ^.*%s.*$
        action: keep
      - source_labels:
          - __meta_kubernetes_pod_label_appName
        action: replace
        target_label: app
      - source_labels:
          - __meta_kubernetes_pod_name
        action: replace
        target_label: pod
      - action: replace
        replacement: /var/log/pods/*$1/*.log
        separator: /
        source_labels:
        - __meta_kubernetes_pod_uid
        - __meta_kubernetes_pod_container_name
        target_label: __path__
`, lokiURL, defaultLokiPort, cfg.Namespace, dep.App.Name)

	// Config is stored as a config map in the daemonset.
	cm := corev1.ConfigMap{
		TypeMeta: metav1.TypeMeta{
			Kind:       "ConfigMap",
			APIVersion: "v1",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      promName,
			Namespace: cfg.Namespace,
		},
		Data: map[string]string{
			"promtail.yaml": config,
		},
	}
	content, err := yaml.Marshal(cm)
	if err != nil {
		return nil, err
	}

	var generated []byte
	generated = append(generated, []byte(fmt.Sprintf("\n# Config Map %s\n", cm.Name))...)
	generated = append(generated, content...)
	generated = append(generated, []byte("\n---\n")...)
	fmt.Fprintf(os.Stderr, "Generated kube deployment for config map %s\n", cm.Name)

	return generated, nil
}

// generateLokiServiceConfigs generates the Loki kubernetes service information
// for a given app.
//
// Note that the configs should be generated iff the kube deployer automatically
// runs a Loki service along with the app.
//
// TODO(rgrandl): We run a single instance of Loki for now. We might want to
// scale it up if it becomes a bottleneck.
func generateLokiServiceConfigs(dep *protos.Deployment, cfg *KubeConfig) ([]byte, error) {
	// Build the kubernetes Loki deployment.
	cname := name{dep.App.Name, "loki", "config"}.DNSLabel()
	lname := name{dep.App.Name, "loki"}.DNSLabel()

	d := &appsv1.Deployment{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "apps/v1",
			Kind:       "Deployment",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      lname,
			Namespace: cfg.Namespace,
		},
		Spec: appsv1.DeploymentSpec{
			Replicas: ptrOf(int32(1)),
			Selector: &metav1.LabelSelector{
				MatchLabels: map[string]string{"loki": lname},
			},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels:    map[string]string{"loki": lname},
					Namespace: cfg.Namespace,
				},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{
							Name:            lname,
							Image:           fmt.Sprintf("%s:latest", autoLokiImageName),
							ImagePullPolicy: corev1.PullIfNotPresent,
							Args: []string{
								fmt.Sprintf("--config.file=/etc/%s/loki.yaml", lname),
							},
							Ports: []corev1.ContainerPort{{ContainerPort: defaultLokiPort}},
							VolumeMounts: []corev1.VolumeMount{
								{
									Name:      cname,
									MountPath: fmt.Sprintf("/etc/%s/", lname),
								},
							},
						},
					},
					Volumes: []corev1.Volume{
						{
							Name: cname,
							VolumeSource: corev1.VolumeSource{
								ConfigMap: &corev1.ConfigMapVolumeSource{
									LocalObjectReference: corev1.LocalObjectReference{
										Name: cname,
									},
									Items: []corev1.KeyToPath{
										{
											Key:  "loki.yaml",
											Path: "loki.yaml",
										},
									},
								},
							},
						},
					},
				},
			},
			Strategy: appsv1.DeploymentStrategy{
				Type:          "RollingUpdate",
				RollingUpdate: &appsv1.RollingUpdateDeployment{},
			},
		},
	}
	content, err := yaml.Marshal(d)
	if err != nil {
		return nil, err
	}

	var generated []byte
	generated = append(generated, []byte(fmt.Sprintf("\n# Loki Deployment %s\n", lname))...)
	generated = append(generated, content...)
	generated = append(generated, []byte("\n---\n")...)
	fmt.Fprintf(os.Stderr, "Generated kube deployment for Loki %s\n", lname)

	// Build the kubernetes Loki service.
	s := &corev1.Service{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "v1",
			Kind:       "Service",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      lname,
			Namespace: cfg.Namespace,
		},
		Spec: corev1.ServiceSpec{
			Selector: map[string]string{"loki": lname},
			Ports: []corev1.ServicePort{
				{
					Port:       defaultLokiPort,
					Protocol:   "TCP",
					TargetPort: intstr.IntOrString{IntVal: int32(defaultLokiPort)},
				},
			},
		},
	}
	content, err = yaml.Marshal(s)
	if err != nil {
		return nil, err
	}
	generated = append(generated, []byte(fmt.Sprintf("\n# Loki Service %s\n", lname))...)
	generated = append(generated, content...)
	generated = append(generated, []byte("\n---\n")...)
	fmt.Fprintf(os.Stderr, "Generated kube service for Loki %s\n", lname)

	return generated, nil
}

// generatePromtailAgentConfigs generates the Promtail kubernetes configs
// to deploy Promtail on each node in the cluster.
//
// Note that the configs should be generated iff the kube deployer automatically
// runs Promtail along with the app.
func generatePromtailAgentConfigs(dep *protos.Deployment, cfg *KubeConfig) ([]byte, error) {
	// Create a Promtail daemonset that will run on each node. The daemonset will
	// run in order to scrape the pods running on each node.
	promName := name{dep.App.Name, "promtail"}.DNSLabel()
	dset := appsv1.DaemonSet{
		TypeMeta: metav1.TypeMeta{
			Kind:       "DaemonSet",
			APIVersion: "apps/v1",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      promName,
			Namespace: cfg.Namespace,
		},
		Spec: appsv1.DaemonSetSpec{
			Selector: &metav1.LabelSelector{
				MatchLabels: map[string]string{
					"promtail": promName,
				},
			},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						"promtail": promName,
					},
					Namespace: cfg.Namespace,
				},
				Spec: corev1.PodSpec{
					ServiceAccountName: "default",
					Containers: []corev1.Container{
						{
							Name:            promName,
							Image:           fmt.Sprintf("%s:latest", autoPromtailImageName),
							ImagePullPolicy: corev1.PullIfNotPresent,
							Args: []string{
								fmt.Sprintf("--config.file=/etc/%s/promtail.yaml", promName),
							},
							Ports: []corev1.ContainerPort{{ContainerPort: 3101}},
							Env: []corev1.EnvVar{
								{
									Name: "HOSTNAME",
									ValueFrom: &corev1.EnvVarSource{
										FieldRef: &corev1.ObjectFieldSelector{
											FieldPath: "spec.nodeName",
										},
									},
								},
							},
							VolumeMounts: []corev1.VolumeMount{
								{
									Name:      "config",
									MountPath: fmt.Sprintf("/etc/%s", promName),
								},
								{
									Name:      "run",
									MountPath: "/run/promtail",
								},
								{
									Name:      "containers",
									MountPath: "/var/lib/docker/containers",
									ReadOnly:  true,
								},
								{
									Name:      "pods",
									MountPath: "/var/log/pods",
									ReadOnly:  true,
								},
							},
						},
					},
					Volumes: []corev1.Volume{
						{
							Name: "config",
							VolumeSource: corev1.VolumeSource{
								ConfigMap: &corev1.ConfigMapVolumeSource{
									LocalObjectReference: corev1.LocalObjectReference{
										Name: promName,
									},
									Items: []corev1.KeyToPath{
										{
											Key:  "promtail.yaml",
											Path: "promtail.yaml",
										},
									},
								},
							},
						},
						{
							Name: "run",
							VolumeSource: corev1.VolumeSource{
								HostPath: &corev1.HostPathVolumeSource{
									Path: "/run/promtail",
								},
							},
						},
						{
							Name: "containers",
							VolumeSource: corev1.VolumeSource{
								HostPath: &corev1.HostPathVolumeSource{
									Path: "/var/lib/docker/containers",
								},
							},
						},
						{
							Name: "pods",
							VolumeSource: corev1.VolumeSource{
								HostPath: &corev1.HostPathVolumeSource{
									Path: "/var/log/pods",
								},
							},
						},
					},
				},
			},
			UpdateStrategy: appsv1.DaemonSetUpdateStrategy{
				Type:          "RollingUpdate",
				RollingUpdate: &appsv1.RollingUpdateDaemonSet{},
			},
		},
	}
	content, err := yaml.Marshal(dset)
	if err != nil {
		return nil, err
	}

	var generated []byte
	generated = append(generated, []byte(fmt.Sprintf("\n# Promtail DaemonSet %s\n", promName))...)
	generated = append(generated, content...)
	generated = append(generated, []byte("\n---\n")...)
	fmt.Fprintf(os.Stderr, "Generated kube daemonset for Promtail %s\n", promName)

	return generated, nil
}

// generateConfigsToExportToGrafana generates the Grafana kubernetes deployment
// information for a given app.
//
// TODO(rgrandl): Convert the below comments into docs.
// How to integrate with an external Grafana service?
// E.g., if you use Helm [1] to install Grafana, you can simply do the following:
// 1) You install Grafana using a command similar to the one below.
// helm install grafana grafana/grafana
//
// Assume your running Grafana service is 'grafana'
//
// 2) In your config.toml file, you should specify the name of your Grafana service:
//
//	config.toml
//	...
//	[kube]
//	observability = {grafana_service = "grafana"}
//
//	Once the Kube deployer generates the deployment information, you should update
//	your grafana release with the datasources and the dashboard from the generated
//	config map for Grafana. You can find the config map by running kubectl cm -n <namespace>.
//	The config map should be named something like collatz-grafana-config-xyz.
//
// 3) Create a manifest file containing the datasource and the dashboard (e.g., graf.yaml).
//
// 4) Update your grafana release as follows:
// helm upgrade grafana grafana/grafana -f graf.yaml
//
// 5) Your Grafana UI should be able to load the Service Weaver dashboard, and
// the configured datasources.
//
// [1] https://helm.sh/
func generateConfigsToExportToGrafana(dep *protos.Deployment, cfg *KubeConfig) ([]byte, error) {
	// The user disabled Grafana, don't generate anything.
	if cfg.Observability[grafanaConfigKey] == disabled {
		return nil, nil
	}

	// Generate configs needed to configure Grafana.
	var generated []byte
	content, err := generateGrafanaConfigs(dep, cfg)
	if err != nil {
		return nil, err
	}
	generated = append(generated, content...)

	// Generate the Grafana kubernetes deployment info iff the kube deployer should
	// deploy the Grafana service.
	if cfg.Observability[grafanaConfigKey] != auto {
		return generated, nil
	}

	// Generate kubernetes service configs needed to run Grafana.
	content, err = generateGrafanaServiceConfigs(dep, cfg)
	if err != nil {
		return nil, err
	}
	generated = append(generated, content...)

	return generated, nil
}

// generateGrafanaConfigs generate configs needed by the Grafana service
// to manipulate various datasources and to export dashboards.
//
// TODO(rgrandl): check if we can simplify the configurations.
func generateGrafanaConfigs(dep *protos.Deployment, cfg *KubeConfig) ([]byte, error) {
	cname := name{dep.App.Name, "grafana", "config"}.DNSLabel()

	// Build the config map that holds the Grafana configuration file. In the
	// config we specify which data source connections the Grafana service should
	// export. By default, we export the Prometheus, Jaeger and Loki services in
	// order to have a single dashboard where we can visualize the metrics and the
	// traces of the app.
	config := `
apiVersion: 1
datasources:
`

	// Set up the Jaeger data source (if any).
	var jaegerURL string
	jservice := cfg.Observability[tracesConfigKey]
	switch {
	case jservice == auto:
		// jaegerURL should point to the Jaeger service generated by the Kube deployer.
		jaegerURL = fmt.Sprintf("http://%s:%d", name{dep.App.Name, jaegerAppName}.DNSLabel(), defaultJaegerUIPort)
	case jservice != disabled:
		// jaegerURL should point to the Jaeger service provided by the user.
		jaegerURL = fmt.Sprintf("http://%s:%d", jservice, defaultJaegerUIPort)
	default:
		// No Jaeger service URL to set.
	}
	if jaegerURL != "" {
		config = fmt.Sprintf(`
%s
 - name: Jaeger
   type: jaeger
   url: %s
`, config, jaegerURL)
	}

	// Set up the Prometheus data source (if any).
	var prometheusURL string
	pservice := cfg.Observability[metricsConfigKey]
	switch {
	case pservice == auto:
		// prometheusURL should point to the Prometheus service generated by the Kube deployer.
		prometheusURL = fmt.Sprintf("http://%s", name{dep.App.Name, "prometheus"}.DNSLabel())
	case pservice != disabled:
		// prometheusURL should point to the Prometheus service provided by the user.
		prometheusURL = fmt.Sprintf("http://%s", pservice)
	default:
		// No Prometheus service URL to set.
	}
	if prometheusURL != "" {
		config = fmt.Sprintf(`
%s
 - name: Prometheus
   type: prometheus
   access: proxy
   url: %s
   isDefault: true
`, config, prometheusURL)
	}

	// Set up the Loki data source (if any).
	var lokiURL string
	lservice := cfg.Observability[logsConfigKey]
	switch {
	case lservice == auto:
		// lokiURL should point to the Loki service generated by the Kube deployer.
		lokiURL = fmt.Sprintf("http://%s:%d", name{dep.App.Name, "loki"}.DNSLabel(), defaultLokiPort)
	case lservice != disabled:
		// lokiURL should point to the Loki service provided by the user.
		lokiURL = fmt.Sprintf("http://%s:%d", lservice, defaultLokiPort)
	default:
		// No Loki service URL to set.
	}
	if lokiURL != "" {
		// Note that we add a custom HTTP header 'X-Scope-OrgID' to make Grafana
		// work with a Loki datasource that runs in multi-tenant mode [1].
		//
		// [1] https://stackoverflow.com/questions/76387302/configuring-loki-datasource-for-grafana-error-no-org-id-found
		//
		// TODO(rgrandl): Investigate how we can do this in a more programmatic way.
		config = fmt.Sprintf(`
%s
 - name: Loki
   type: loki
   access: proxy
   jsonData:
     httpHeaderName1: 'X-Scope-OrgID'
   secureJsonData:
     httpHeaderValue1: 'customvalue'
   url: %s
`, config, lokiURL)
	}

	// It contains the list of dashboard providers that load dashboards into
	// Grafana from the local filesystem [1].
	//
	// https://grafana.com/docs/grafana/latest/administration/provisioning/#dashboards
	const dashboard = `
apiVersion: 1
providers:
 - name: 'Service Weaver Dashboard'
   options:
     path: /etc/grafana/dashboards/default-dashboard.json
`

	// Create a config map to store the Grafana configs and the default dashboards.
	cm := corev1.ConfigMap{
		TypeMeta: metav1.TypeMeta{
			Kind:       "ConfigMap",
			APIVersion: "v1",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      cname,
			Namespace: cfg.Namespace,
		},
		Data: map[string]string{
			"grafana.yaml":           config,
			"dashboard-config.yaml":  dashboard,
			"default-dashboard.json": fmt.Sprintf(dashboardContent, dep.App.Name),
		},
	}
	content, err := yaml.Marshal(cm)
	if err != nil {
		return nil, err
	}
	var generated []byte
	generated = append(generated, []byte(fmt.Sprintf("\n# Config Map %s\n", cname))...)
	generated = append(generated, content...)
	generated = append(generated, []byte("\n---\n")...)
	fmt.Fprintf(os.Stderr, "Generated kube deployment for config map %s\n", cname)

	return generated, nil
}

// generateGrafanaServiceConfigs generates the kubernetes configurations to deploy
// a Grafana service for a given app.
//
// Note that the configs should be generated iff the kube deployer automatically
// runs Grafana along with the app.
//
// TODO(rgrandl): We run a single instance of Grafana for now. We might want
// to scale it up if it becomes a bottleneck.
func generateGrafanaServiceConfigs(dep *protos.Deployment, cfg *KubeConfig) ([]byte, error) {
	cname := name{dep.App.Name, "grafana", "config"}.DNSLabel()
	gname := name{dep.App.Name, "grafana"}.DNSLabel()

	// Generate the Grafana deployment.
	d := &appsv1.Deployment{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "apps/v1",
			Kind:       "Deployment",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      gname,
			Namespace: cfg.Namespace,
		},
		Spec: appsv1.DeploymentSpec{
			Replicas: ptrOf(int32(1)),
			Selector: &metav1.LabelSelector{
				MatchLabels: map[string]string{"grafana": gname},
			},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels:    map[string]string{"grafana": gname},
					Namespace: cfg.Namespace,
				},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{
							Name:            gname,
							Image:           fmt.Sprintf("%s:latest", autoGrafanaImageName),
							ImagePullPolicy: corev1.PullIfNotPresent,
							Ports:           []corev1.ContainerPort{{ContainerPort: defaultGrafanaPort}},
							VolumeMounts: []corev1.VolumeMount{
								{
									// By default, we have to store any data source connection that
									// should be exported by Grafana under provisioning/datasources.
									Name:      "datasource-volume",
									MountPath: "/etc/grafana/provisioning/datasources/",
								},
								{
									// By default, we need to store the dashboards config files under
									// provisioning/dashboards directory. Each config file can contain
									// a list of dashboards providers that load dashboards into Grafana
									// from the local filesystem. More info here [1].
									//
									// [1] https://grafana.com/docs/grafana/latest/administration/provisioning/#dashboards
									Name:      "dashboards-config",
									MountPath: "/etc/grafana/provisioning/dashboards/",
								},
								{
									// Mount the volume that stores the predefined dashboards.
									Name:      "dashboards",
									MountPath: "/etc/grafana/dashboards/",
								},
							},
							Env: []corev1.EnvVar{
								// TODO(rgrandl): we may want to enable the user to specify their
								// credentials in a different way.
								{
									Name:  "GF_SECURITY_ADMIN_USER",
									Value: "admin",
								},
								{
									Name:  "GF_SECURITY_ADMIN_PASSWORD",
									Value: "admin",
								},
							},
						},
					},
					Volumes: []corev1.Volume{
						{
							Name: "datasource-volume",
							VolumeSource: corev1.VolumeSource{
								ConfigMap: &corev1.ConfigMapVolumeSource{
									LocalObjectReference: corev1.LocalObjectReference{
										Name: cname,
									},
									Items: []corev1.KeyToPath{
										{
											Key:  "grafana.yaml",
											Path: "grafana.yaml",
										},
									},
								},
							},
						},
						{
							Name: "dashboards-config",
							VolumeSource: corev1.VolumeSource{
								ConfigMap: &corev1.ConfigMapVolumeSource{
									LocalObjectReference: corev1.LocalObjectReference{
										Name: cname,
									},
									Items: []corev1.KeyToPath{
										{
											Key:  "dashboard-config.yaml",
											Path: "dashboard-config.yaml",
										},
									},
								},
							},
						},
						{
							Name: "dashboards",
							VolumeSource: corev1.VolumeSource{
								ConfigMap: &corev1.ConfigMapVolumeSource{
									LocalObjectReference: corev1.LocalObjectReference{
										Name: cname,
									},
									Items: []corev1.KeyToPath{
										{
											Key:  "default-dashboard.json",
											Path: "default-dashboard.json",
										},
									},
								},
							},
						},
					},
				},
			},
			Strategy: appsv1.DeploymentStrategy{
				Type:          "RollingUpdate",
				RollingUpdate: &appsv1.RollingUpdateDeployment{},
			},
		},
	}
	content, err := yaml.Marshal(d)
	if err != nil {
		return nil, err
	}

	var generated []byte
	generated = append(generated, []byte("# Grafana Deployment\n")...)
	generated = append(generated, content...)
	generated = append(generated, []byte("\n---\n")...)
	fmt.Fprintf(os.Stderr, "Generated Grafana deployment\n")

	// Generate the Grafana service.
	//
	// TODO(rgrandl): should we create a load balancer instead of a cluster ip?
	s := &corev1.Service{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "v1",
			Kind:       "Service",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      gname,
			Namespace: cfg.Namespace,
		},
		Spec: corev1.ServiceSpec{
			Selector: map[string]string{"grafana": gname},
			Ports: []corev1.ServicePort{
				{
					Name:       "ui-port",
					Port:       servicePort,
					Protocol:   "TCP",
					TargetPort: intstr.IntOrString{IntVal: int32(defaultGrafanaPort)},
				},
			},
		},
	}
	content, err = yaml.Marshal(s)
	if err != nil {
		return nil, err
	}
	generated = append(generated, []byte("\n# Grafana Service\n")...)
	generated = append(generated, content...)
	generated = append(generated, []byte("\n---\n")...)
	fmt.Fprintf(os.Stderr, "Generated Grafana service\n")

	return generated, nil
}
