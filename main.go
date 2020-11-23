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
	Kind   string
	Desc   string
	Hosts  []ResultHost
	Ports  []ResultPort
	Color  string
	Access string
}

type ResultHost struct {
	DNS string
	IP  string
}

type ResultPort struct {
	From     string
	To       string
	Protocol string
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

func convertServiceToResultPorts(service corev1.Service, nodePort bool) (ports []ResultPort) {
	for _, port := range service.Spec.Ports {
		var sourcePort string
		if nodePort {
			sourcePort = fmt.Sprintf("%d", port.NodePort)
		} else {
			sourcePort = fmt.Sprintf("%d", port.Port)
		}
		ports = append(ports, ResultPort{
			From:     sourcePort,
			To:       port.TargetPort.String(),
			Protocol: string(port.Protocol),
		})
	}
	return
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

	ep := ResultEndpoint{
		Kind:   "HostNetwork",
		Desc:   "容器直接运行在宿主机网络栈上",
		Hosts:  []ResultHost{{}},
		Ports:  []ResultPort{{}},
		Color:  "danger",
		Access: "集群内网可访问,腾讯云内网可访问",
	}

	if hostname != "" {
		for _, node := range nodes.Items {
			if node.Name == hostname {
				ip := getNodeIP(node)
				ep.Hosts[0] = ResultHost{
					DNS: node.Name,
					IP:  ip,
				}
			}
		}
	}

	workload.Endpoints = append(workload.Endpoints, ep)
}

func scanTKEFixedIP(workload *ResultWorkload, namespace string, pts corev1.PodTemplateSpec, pods *corev1.PodList, podSelector map[string]string) {
	if pts.Annotations != nil &&
		pts.Annotations["tke.cloud.tencent.com/vpc-ip-claim-delete-policy"] == "Never" {
		var hosts []ResultHost
		for _, pod := range pods.Items {
			if pod.Namespace != namespace {
				continue
			}
			if matchSelector(podSelector, pod.Labels) {
				if pod.Status.PodIP != "" {
					hosts = append(hosts, ResultHost{IP: pod.Status.PodIP})
				}
			}
		}
		if len(hosts) != 0 {
			workload.Endpoints = append(workload.Endpoints, ResultEndpoint{
				Kind:   "固定 PodIP",
				Desc:   "TKE 集群特有功能，允许 Statefulset 类型工作负载的 Pod 重建后 IP 不发生变化",
				Hosts:  hosts,
				Color:  "success",
				Access: "腾讯云内网可访问",
			})
		}
	}
}

func scanService(workload *ResultWorkload, namespace string, pts corev1.PodTemplateSpec, nodePortHosts []ResultHost, services *corev1.ServiceList) {
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
					Kind: "PodIP",
					Desc: "仅在集群内网可访问有效，可以访问全部端口，随 Pod 重建而变化，必须每次使用集群内网域名进行解析",
					Hosts: []ResultHost{
						{
							DNS: service.Name + "." + service.Namespace,
						},
					},
					Color:  "secondary",
					Access: "集群内网可访问",
				})
			} else {
				workload.Endpoints = append(workload.Endpoints, ResultEndpoint{
					Kind: "ClusterIP",
					Desc: "仅在集群内网可访问有效，必须声明工作负载端口，不随 Pod 重建而变化，建议使用集群内网域名进行解析",
					Hosts: []ResultHost{
						{
							IP:  service.Spec.ClusterIP,
							DNS: service.Name + "." + service.Namespace,
						},
					},
					Ports:  convertServiceToResultPorts(service, false),
					Color:  "secondary",
					Access: "集群内网可访问",
				})
			}
		}
		if len(nodePortHosts) != 0 && service.Spec.Type == corev1.ServiceTypeNodePort {
			workload.Endpoints = append(workload.Endpoints, ResultEndpoint{
				Kind:   "NodePort",
				Desc:   "集群所有宿主机的某个端口，都指向该工作负载服务的特定端口，必须声明工作负载端口，宿主机端口建议随机防止冲突，访问时任选一个主机 IP 即可",
				Hosts:  nodePortHosts,
				Ports:  convertServiceToResultPorts(service, true),
				Color:  "success",
				Access: "腾讯云内网可访问",
			})
		}
		if service.Spec.Type == corev1.ServiceTypeLoadBalancer {
			if service.Annotations != nil && service.Annotations["service.kubernetes.io/loadbalance-id"] != "" {
				var hosts []ResultHost
				for _, ing := range service.Status.LoadBalancer.Ingress {
					hosts = append(hosts, ResultHost{IP: ing.IP, DNS: ing.Hostname})
				}
				if len(hosts) > 0 {
					workload.Endpoints = append(workload.Endpoints, ResultEndpoint{
						Kind:   "腾讯云L4负载均衡",
						Desc:   "TKE 特有功能",
						Hosts:  hosts,
						Ports:  convertServiceToResultPorts(service, false),
						Color:  "success",
						Access: "腾讯云内网可访问",
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

	var nodePortHosts []ResultHost

	for _, node := range nodes.Items {
		sfnp := false
		if node.Annotations != nil {
			sfnp, _ = strconv.ParseBool(node.Annotations["autoops/suitable-for-nodeport"])
		}
		ip := getNodeIP(node)
		if sfnp {
			nodePortHosts = append(nodePortHosts, ResultHost{
				IP: ip,
			})
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
			scanService(&workload, deployment.Namespace, deployment.Spec.Template, nodePortHosts, services)

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
			scanService(&workload, statefulset.Namespace, statefulset.Spec.Template, nodePortHosts, services)

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
