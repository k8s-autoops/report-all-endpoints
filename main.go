package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"html/template"
	"io/ioutil"
	appv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	extensionsv1beta1 "k8s.io/api/extensions/v1beta1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"log"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

const (
	TKEHTTPRules    = "kubernetes.io/ingress.http-rules"
	TKEHTTPsRules   = "kubernetes.io/ingress.https-rules"
	TKEDirectAccess = "ingress.cloud.tencent.com/direct-access"
)

var (
	hiddenNamespaces = []string{
		"autoops",
		"cattle-prometheus",
		"cattle-system",
		"kube-system",
		"kube-public",
		"kube-node-lease",
		"nginx-ingress",
		"ingress-nginx",
		"nfs-client-provisioner",
	}
)

func exit(err *error) {
	if *err != nil {
		log.Println("exited with error:", (*err).Error())
		os.Exit(1)
	}
}

type HTTPRule struct {
	Host    string `json:"host,omitempty"`
	Path    string `json:"path,omitempty"`
	Backend struct {
		ServiceName string `json:"serviceName"`
		ServicePort string `json:"servicePort"`
	} `json:"backend,omitempty"`
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
	Description string
	Notice      template.HTML
	Hosts       []ResultHost
	Ports       []ResultPort
	Color       string
	Access      string
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

func renderList(items ...string) string {
	sb := &strings.Builder{}
	//sb.WriteString("<ul>")
	for _, item := range items {
		sb.WriteString("<li>")
		sb.WriteString(item)
		sb.WriteString("</li>")
	}
	//sb.WriteString("</ul>")
	return sb.String()
}

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
		Kind:        "HostNetwork",
		Description: renderList("容器直接运行在宿主机网络栈上"),
		Hosts:       []ResultHost{{}},
		Ports:       []ResultPort{{}},
		Color:       "danger",
		Access:      "集群内网访问,腾讯云内网访问",
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
				Kind:        "PodIP (固定)",
				Description: renderList("TKE 集群特有功能", "允许 Statefulset 类型工作负载的 Pod 重建后 IP 不发生变化"),
				Hosts:       hosts,
				Color:       "success",
				Access:      "腾讯云内网访问",
			})
		}
	}
}

func scanService(workload *ResultWorkload, namespace string, pts corev1.PodTemplateSpec, nodePortHosts []ResultHost, services *corev1.ServiceList, pods *corev1.PodList, ingresses *extensionsv1beta1.IngressList) {
	// 依据 Pod 来扩展 Labels，这个是 Rancher 特有的工作模式
	expandedLabels := map[string]string{}

	for k, v := range pts.Labels {
		expandedLabels[k] = v
	}

	for _, pod := range pods.Items {
		if pod.Namespace != namespace {
			continue
		}
		if !matchSelector(pts.Labels, pod.Labels) {
			continue
		}
		for k, v := range pod.Labels {
			if strings.HasPrefix(k, "workloadID_ingress-") {
				expandedLabels[k] = v
			}
		}
	}

	for _, service := range services.Items {
		if service.Namespace != namespace {
			continue
		}

		if !matchSelector(service.Spec.Selector, expandedLabels) {
			continue
		}

		// 无法使用标准 Labels 匹配，但是可以使用扩展 Labels 匹配，说明 扩展 Labels 与标准 Labels 不一样
		matchByExpanded := !matchSelector(service.Spec.Selector, pts.Labels)

		if service.Spec.Type == corev1.ServiceTypeClusterIP {
			if service.Spec.ClusterIP == corev1.ClusterIPNone {
				// Headless ClusterIP 模式
				workload.Endpoints = append(workload.Endpoints, ResultEndpoint{
					Kind:        "PodIP",
					Notice:      "建议添加 ClusterIP 端口规则，提供更稳定的集群内网连接",
					Description: renderList("IP 随 Pod 重建而变化，必须每次使用集群内网域名进行解析", "可以访问全部端口"),
					Hosts: []ResultHost{
						{
							DNS: service.Name + "." + service.Namespace,
						},
					},
					Color:  "secondary",
					Access: "集群内网访问",
				})
			} else {
				// 正常 ClusterIP 模式
				// 忽略 Rancher 扩展的 ClusterIP 服务
				if !matchByExpanded {
					workload.Endpoints = append(workload.Endpoints, ResultEndpoint{
						Kind:        "ClusterIP",
						Description: renderList("IP 不随 Pod 重建而变化，仍然建议使用集群内网域名进行解析", "只能访问声明的端口"),
						Hosts: []ResultHost{
							{
								IP:  service.Spec.ClusterIP,
								DNS: service.Name + "." + service.Namespace,
							},
						},
						Ports:  convertServiceToResultPorts(service, false),
						Color:  "secondary",
						Access: "集群内网访问",
					})
				}

				// 查询 Ingress 条目
				for _, ingress := range ingresses.Items {
					if ingress.Namespace != namespace {
						continue
					}

					for _, rule := range ingress.Spec.Rules {
						if rule.IngressRuleValue.HTTP == nil {
							continue
						}
						scheme := "http://"
						sourcePort := "80"
						for _, tls := range ingress.Spec.TLS {
							for _, host := range tls.Hosts {
								if host == rule.Host {
									scheme = "https://"
									sourcePort = "443"
								}
							}
						}

						for _, path := range rule.HTTP.Paths {
							if path.Backend.ServiceName == service.Name {
								if matchByExpanded {
									workload.Endpoints = append(workload.Endpoints, ResultEndpoint{
										Kind:        "Ingress (Rancher)",
										Description: renderList("Rancher 特有的 Ingress 条目", "添加 Ingress 记录时自动生成 ClusterIP 条目"),
										Notice:      `正常情况下，创建 Ingress 条目<b>必须先创建</b> ClusterIP 端口规则，并<b>只能选择</b>声明的端口<br/>虽然 Rancher 的 <b>负载均衡</b> 页面允许你直接添加 <b>工作负载</b> 并填写 <b>任意端口</b>，但这会<b>自动产生冗余的 ClusterIP 条目</b><br/>建议<b>不要</b>在 <b>负载均衡</b> 页面使用 <b>[+负载均衡]</b> 按钮，改为手动为 <b>工作负载</b> 配置 <b>ClusterIP 端口规则</b>，然后使用 <b>[+服务]</b> 按钮`,
										Hosts: []ResultHost{
											{
												DNS: scheme + rule.Host + path.Path,
											},
										},
										Ports: []ResultPort{
											{
												From:     sourcePort,
												To:       path.Backend.ServicePort.String(),
												Protocol: string(corev1.ProtocolTCP),
											},
										},
										Color:  "warning",
										Access: "公网访问",
									})
								} else {
									workload.Endpoints = append(workload.Endpoints, ResultEndpoint{
										Kind:        "Ingress",
										Description: renderList("Ingress 代表 L7 负载均衡条目，基于<b>域名</b>和<b>路径前缀</b>路由到不同的工作负载", "根据集群部署方式不同，集群会针对 Ingress 规则，更新集群入口 Nginx 配置，或者生成腾讯云公网负载均衡"),
										Hosts: []ResultHost{
											{
												DNS: scheme + rule.Host + path.Path,
											},
										},
										Ports: []ResultPort{
											{
												From:     sourcePort,
												To:       path.Backend.ServicePort.String(),
												Protocol: string(corev1.ProtocolTCP),
											},
										},
										Color:  "warning",
										Access: "公网访问",
									})
								}
							}
						}
					}
				}
			}
		}
		if service.Spec.Type == corev1.ServiceTypeNodePort {
			// 查询腾讯云特有的 Ingress 条目
			for _, ingress := range ingresses.Items {
				if ingress.Namespace != service.Namespace {
					continue
				}
				if ingress.Annotations != nil &&
					(ingress.Annotations[TKEHTTPRules] != "" || ingress.Annotations[TKEHTTPsRules] != "") {

					dr, _ := strconv.ParseBool(ingress.Annotations[TKEDirectAccess])

					collectRules := func(rules []HTTPRule, https bool) {
						for _, rule := range rules {
							if rule.Backend.ServiceName == service.Name {
								var (
									ip     string
									kind   string
									color  string
									access string
								)
								if len(ingress.Status.LoadBalancer.Ingress) > 0 {
									ip = ingress.Status.LoadBalancer.Ingress[0].IP
								}
								if ip == "" {
									kind = "Ingress (TKE L7 负载均衡)"
									color = "secondary"
									access = "公网 或 腾讯云内网访问"
								} else {
									if isPrivateIP(net.ParseIP(ip)) {
										kind = "Ingress (TKE L7 内网负载均衡)"
										color = "success"
										access = "腾讯云内网访问"
									} else {
										kind = "Ingress (TKE L7 公网负载均衡)"
										color = "warning"
										access = "公网访问"
									}
								}
								if dr {
									kind = kind + " [直连]"
								}
								host := rule.Host
								if host == "" {
									host = ip
								}
								scheme := "http://"
								if https {
									scheme = "https://"
								}
								port := "80"
								if https {
									port = "443"
								}
								workload.Endpoints = append(workload.Endpoints, ResultEndpoint{
									Kind:        kind,
									Notice:      `需要提前创建 NodePort 类型的端口规则，必须在腾讯云管理台进行维护，请勿在 Rancher 管理台维护`,
									Description: renderList("TKE 特有功能", "必须在腾讯云管理台维护"),
									Hosts: []ResultHost{
										{
											DNS: scheme + host + rule.Path,
											IP:  ip,
										},
									},
									Ports: []ResultPort{
										{
											From:     port,
											To:       rule.Backend.ServicePort,
											Protocol: string(corev1.ProtocolTCP),
										},
									},
									Color:  color,
									Access: access,
								})
							}
						}
					}

					var rules []HTTPRule
					if ingress.Annotations[TKEHTTPRules] != "" {
						if err := json.Unmarshal([]byte(ingress.Annotations[TKEHTTPRules]), &rules); err != nil {
							log.Println("遭遇 TKE Ingress http-rules JSON解析错误:", err.Error())
						} else {
							collectRules(rules, false)
						}
					}
					rules = nil
					if ingress.Annotations[TKEHTTPsRules] != "" {
						if err := json.Unmarshal([]byte(ingress.Annotations[TKEHTTPsRules]), &rules); err != nil {
							log.Println("遭遇 TKE Ingress http-rules JSON解析错误:", err.Error())
						} else {
							collectRules(rules, true)
						}
					}
				}
			}

			if len(nodePortHosts) > 0 {
				workload.Endpoints = append(workload.Endpoints, ResultEndpoint{
					Kind:        "NodePort",
					Description: renderList("将集群<b>所有</b>宿主机的指定端口映射到<b>工作负载</b>的指定端口", "创建 NodePort 端口规则时，宿主机端口建议随机选择以防止冲突", "访问时任选一个主机 IP 即可"),
					Hosts:       nodePortHosts,
					Ports:       convertServiceToResultPorts(service, true),
					Color:       "success",
					Access:      "腾讯云内网访问",
				})
			}
		}

		if service.Spec.Type == corev1.ServiceTypeLoadBalancer {
			if service.Annotations != nil && service.Annotations["service.kubernetes.io/loadbalance-id"] != "" {
				var hosts []ResultHost
				for _, ing := range service.Status.LoadBalancer.Ingress {
					hosts = append(hosts, ResultHost{IP: ing.IP, DNS: ing.Hostname})
				}
				if len(hosts) > 0 {
					workload.Endpoints = append(workload.Endpoints, ResultEndpoint{
						Kind:        "腾讯云L4负载均衡",
						Description: renderList("TKE 特有功能"),
						Hosts:       hosts,
						Ports:       convertServiceToResultPorts(service, false),
						Color:       "success",
						Access:      "腾讯云内网访问",
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
	var ingresses *extensionsv1beta1.IngressList
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

outerLoop:
	for _, _namespace := range namespaces.Items {
		for _, hns := range hiddenNamespaces {
			if strings.HasPrefix(_namespace.Name, hns) {
				continue outerLoop
			}
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
			scanService(&workload, deployment.Namespace, deployment.Spec.Template, nodePortHosts, services, pods, ingresses)

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
			scanService(&workload, statefulset.Namespace, statefulset.Spec.Template, nodePortHosts, services, pods, ingresses)

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
