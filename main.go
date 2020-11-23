package main

import (
	"bytes"
	"context"
	"flag"
	"fmt"
	"html/template"
	"io/ioutil"
	appv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/api/extensions/v1beta1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

var (
	hiddenNamespaces = map[string]bool{
		"autoops":                true,
		"cattle-prometheus":      true,
		"cattle-system":          true,
		"kube-system":            true,
		"kube-public":            true,
		"kube-node-lease":        true,
		"nginx-ingress":          true,
		"ingress-nginx":          true,
		"nfs-client-provisioner": true,
	}
)

func exit(err *error) {
	if *err != nil {
		log.Println("exited with error:", (*err).Error())
		os.Exit(1)
	}
}

type Result struct {
	GeneratedAt string
	Clusters    []ResultCluster
}

type ResultCluster struct {
	Name        string
	Description template.HTML
	Nodes       []ResultNode
	Namespaces  []ResultNamespace
}

type ResultNode struct {
	Name                string
	IP                  string
	SuitableForNodePort bool
}

type ResultNamespace struct {
	Name      string
	Workloads []ResultWorkload
}

type ResultWorkload struct {
	Name      string
	Namespace string
	Kind      string
	Replicas  int
	Endpoints []ResultEndpoint
}

type ResultEndpoint struct {
	Kind        string
	Address     string
	Port        string
	AccessColor string
	Access      string
}

var (
	optConfDir  string
	optOutput   string
	optTemplate string
)

func matchSelector(selector, labels map[string]string) bool {
	if selector == nil {
		return false
	}
	for k, v := range selector {
		if labels[k] != v {
			return false
		}
	}
	return true
}

func scanHostNetwork(workload *ResultWorkload, pts corev1.PodTemplateSpec, nodes *corev1.NodeList) {
	if !pts.Spec.HostNetwork {
		return
	}
	var hostname string
	if pts.Spec.NodeName != "" {
		hostname = pts.Spec.NodeName
	}
	if pts.Spec.Affinity != nil {
		if pts.Spec.Affinity.NodeAffinity != nil {
			if pts.Spec.Affinity.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution != nil {
				if len(pts.Spec.Affinity.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution.NodeSelectorTerms) == 1 {
					nst := pts.Spec.Affinity.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution.NodeSelectorTerms[0]
					if len(nst.MatchExpressions) == 1 {
						me := nst.MatchExpressions[0]
						if me.Key == "kubernetes.io/hostname" && me.Operator == corev1.NodeSelectorOpIn && len(me.Values) == 1 {
							hostname = me.Values[0]
						}
					}
				}
			}
		}
	}
	if hostname != "" {
		for _, node := range nodes.Items {
			if node.Name == hostname {
				ip := getNodeIP(node)
				workload.Endpoints = append(workload.Endpoints, ResultEndpoint{
					Kind:        "宿主机网络",
					Address:     ip + " (" + node.Name + ")",
					Port:        "端口未知，详见具体工作负载内部配置",
					AccessColor: "danger",
					Access:      "集群内,腾讯云内网",
				})
				return
			}
		}
	}

	workload.Endpoints = append(workload.Endpoints, ResultEndpoint{
		Kind:        "宿主机网络",
		Address:     "主机未知，详见具体工作负载调度配置",
		Port:        "端口未知，详见具体工作负载内部配置",
		AccessColor: "danger",
		Access:      "集群内,腾讯云内网",
	})
}

func scanTKEFixedIP(workload *ResultWorkload, namespace string, pts corev1.PodTemplateSpec, pods *corev1.PodList, podSelector map[string]string) {
	if pts.Annotations != nil &&
		pts.Annotations["tke.cloud.tencent.com/vpc-ip-claim-delete-policy"] == "Never" {
		var podIPs []string
		for _, pod := range pods.Items {
			if pod.Namespace != namespace {
				continue
			}
			if matchSelector(podSelector, pod.Labels) {
				if pod.Status.PodIP != "" {
					podIPs = append(podIPs, pod.Status.PodIP)
				}
			}
		}
		if len(podIPs) != 0 {
			workload.Endpoints = append(workload.Endpoints, ResultEndpoint{
				Kind:        "Pod IP (固定)",
				Address:     strings.Join(podIPs, ", "),
				AccessColor: "success",
				Access:      "腾讯云内网",
			})
		}
	}
}

func scanService(workload *ResultWorkload, namespace string, pts corev1.PodTemplateSpec, nodePortNodes []string, services *corev1.ServiceList) {
	for _, service := range services.Items {
		if service.Namespace != namespace {
			continue
		}
		if !matchSelector(service.Spec.Selector, pts.Labels) {
			continue
		}
		if service.Spec.Type == corev1.ServiceTypeClusterIP {
			if service.Spec.ClusterIP == corev1.ClusterIPNone {
				workload.Endpoints = append(workload.Endpoints, ResultEndpoint{
					Kind:        "Pod IP (通过集群内 DNS 枚举)",
					Address:     service.Name + "." + service.Namespace,
					AccessColor: "secondary",
					Access:      "集群内",
				})
			} else {
				var ports []string
				for _, port := range service.Spec.Ports {
					ports = append(ports, fmt.Sprintf("%d/%s->%s/%s", port.Port, port.Protocol, port.TargetPort.String(), port.Protocol))
				}
				workload.Endpoints = append(workload.Endpoints, ResultEndpoint{
					Kind:        "Cluster IP",
					Address:     service.Spec.ClusterIP,
					Port:        strings.Join(ports, ", "),
					AccessColor: "secondary",
					Access:      "集群内",
				})
				workload.Endpoints = append(workload.Endpoints, ResultEndpoint{
					Kind:        "Cluster IP (通过集群内 DNS 解析)",
					Address:     service.Name + "." + service.Namespace,
					Port:        strings.Join(ports, ", "),
					AccessColor: "secondary",
					Access:      "集群内",
				})
			}
		}
		if len(nodePortNodes) != 0 && service.Spec.Type == corev1.ServiceTypeNodePort {
			var ports []string
			for _, port := range service.Spec.Ports {
				ports = append(ports, fmt.Sprintf("%d/%s->%s/%s", port.NodePort, port.Protocol, port.TargetPort.String(), port.Protocol))
			}
			workload.Endpoints = append(workload.Endpoints, ResultEndpoint{
				Kind:        "NodePort",
				Address:     strings.Join(nodePortNodes, ", ") + " 任选一个",
				Port:        strings.Join(ports, ", "),
				AccessColor: "success",
				Access:      "腾讯云内网",
			})
		}
		if service.Spec.Type == corev1.ServiceTypeLoadBalancer {
			if service.Annotations != nil && service.Annotations["service.kubernetes.io/loadbalance-id"] != "" {
				var ports []string
				for _, port := range service.Spec.Ports {
					ports = append(ports, fmt.Sprintf("%d/%s->%s/%s", port.Port, port.Protocol, port.TargetPort.String(), port.Protocol))
				}
				var ips []string
				for _, ing := range service.Status.LoadBalancer.Ingress {
					ips = append(ips, ing.IP)
				}
				if len(ips) > 0 {
					workload.Endpoints = append(workload.Endpoints, ResultEndpoint{
						Kind:        "腾讯云 L4 负载均衡",
						Address:     strings.Join(ips, ", "),
						Port:        strings.Join(ports, ", "),
						AccessColor: "success",
						Access:      "腾讯云内网",
					})
				}
			}
		}
	}
}

func getNodeIP(node corev1.Node) string {
	if node.Annotations != nil {
		var k string
		if k = node.Annotations["rke.cattle.io/internal-ip"]; k != "" {
			return k
		}
		if k = node.Annotations["flannel.alpha.coreos.com/public-ip"]; k != "" {
			return k
		}
	}
	return node.Name
}

func generateSummary(kubeconfig, name, desc string) (rc ResultCluster, err error) {
	rc.Name = name
	rc.Description = template.HTML(desc)
	var config *rest.Config
	if config, err = clientcmd.BuildConfigFromFlags("", kubeconfig); err != nil {
		return
	}
	var client *kubernetes.Clientset
	if client, err = kubernetes.NewForConfig(config); err != nil {
		return
	}
	var nodes *corev1.NodeList
	if nodes, err = client.CoreV1().Nodes().List(context.Background(), metav1.ListOptions{}); err != nil {
		return
	}

	var nodePortNodes []string

	for _, node := range nodes.Items {
		sfnp := false
		if node.Annotations != nil {
			sfnp, _ = strconv.ParseBool(node.Annotations["autoops/suitable-for-nodeport"])
		}
		ip := getNodeIP(node)
		if sfnp {
			nodePortNodes = append(nodePortNodes, ip)
		}
		rc.Nodes = append(rc.Nodes, ResultNode{
			Name:                node.Name,
			IP:                  ip,
			SuitableForNodePort: sfnp,
		})
	}

	var namespaces *corev1.NamespaceList
	if namespaces, err = client.CoreV1().Namespaces().List(context.Background(), metav1.ListOptions{}); err != nil {
		return
	}
	var services *corev1.ServiceList
	if services, err = client.CoreV1().Services("").List(context.Background(), metav1.ListOptions{}); err != nil {
		return
	}
	var ingresses *v1beta1.IngressList
	if ingresses, err = client.ExtensionsV1beta1().Ingresses("").List(context.Background(), metav1.ListOptions{}); err != nil {
		return
	}
	var deployments *appv1.DeploymentList
	if deployments, err = client.AppsV1().Deployments("").List(context.Background(), metav1.ListOptions{}); err != nil {
		return
	}
	var statefulsets *appv1.StatefulSetList
	if statefulsets, err = client.AppsV1().StatefulSets("").List(context.Background(), metav1.ListOptions{}); err != nil {
		return
	}
	var pods *corev1.PodList
	if pods, err = client.CoreV1().Pods("").List(context.Background(), metav1.ListOptions{}); err != nil {
		return
	}

	_ = ingresses

	for _, _namespace := range namespaces.Items {
		if hiddenNamespaces[_namespace.Name] {
			continue
		}
		var namespace ResultNamespace
		namespace.Name = _namespace.Name

		for _, deployment := range deployments.Items {
			if deployment.Namespace != _namespace.Name {
				continue
			}
			var workload ResultWorkload
			workload.Name = deployment.Name
			workload.Namespace = deployment.Namespace
			workload.Kind = "Deployment"
			workload.Replicas = int(*deployment.Spec.Replicas)

			scanHostNetwork(&workload, deployment.Spec.Template, nodes)
			scanService(&workload, deployment.Namespace, deployment.Spec.Template, nodePortNodes, services)

			namespace.Workloads = append(namespace.Workloads, workload)
		}

		for _, statefulset := range statefulsets.Items {
			if statefulset.Namespace != _namespace.Name {
				continue
			}
			var workload ResultWorkload
			workload.Name = statefulset.Name
			workload.Namespace = statefulset.Namespace
			workload.Kind = "Statefulset"
			workload.Replicas = int(*statefulset.Spec.Replicas)

			scanHostNetwork(&workload, statefulset.Spec.Template, nodes)
			scanTKEFixedIP(&workload, statefulset.Namespace, statefulset.Spec.Template, pods, statefulset.Spec.Selector.MatchLabels)
			scanService(&workload, statefulset.Namespace, statefulset.Spec.Template, nodePortNodes, services)

			namespace.Workloads = append(namespace.Workloads, workload)
		}

		rc.Namespaces = append(rc.Namespaces, namespace)
	}
	return
}

func main() {
	var err error
	defer exit(&err)

	flag.StringVar(&optConfDir, "conf-dir", "kubeconfig.d", "Kubeconfig 文件目录，每个 yml 文件代表一个集群，使用 .desc.html 文件追加描述")
	flag.StringVar(&optTemplate, "template", "template.html", "模板文件")
	flag.StringVar(&optOutput, "output", "result.html", "要输出的 HTML 文件名")
	flag.Parse()

	var result Result

	result.GeneratedAt = time.Now().Format(time.RFC3339)

	var fis []os.FileInfo
	if fis, err = ioutil.ReadDir(optConfDir); err != nil {
		return
	}

	for _, fi := range fis {
		if fi.IsDir() {
			continue
		}
		if !strings.HasSuffix(strings.ToLower(fi.Name()), ".yml") {
			continue
		}
		name := strings.TrimSuffix(strings.ToLower(fi.Name()), ".yml")
		log.Println("读取集群信息：", name)
		var desc []byte
		if desc, err = ioutil.ReadFile(filepath.Join(optConfDir, name+".desc.html")); err != nil {
			return
		}
		var rc ResultCluster
		if rc, err = generateSummary(filepath.Join(optConfDir, fi.Name()), name, string(desc)); err != nil {
			return
		}
		result.Clusters = append(result.Clusters, rc)
	}

	var buf []byte
	if buf, err = ioutil.ReadFile(optTemplate); err != nil {
		return
	}
	log.Println("读取模板:", optTemplate)

	var tmpl *template.Template
	if tmpl, err = template.New("").Parse(string(buf)); err != nil {
		return
	}
	log.Println("载入模板：", optTemplate)

	var out = &bytes.Buffer{}

	if err = tmpl.Execute(out, result); err != nil {
		return
	}
	log.Println("渲染模板：", optTemplate)

	if err = ioutil.WriteFile(optOutput, out.Bytes(), 0640); err != nil {
		return
	}
	log.Println("写入文件:", optOutput)
}
