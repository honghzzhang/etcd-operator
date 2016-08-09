package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"net/http"
	"strings"

	"k8s.io/kubernetes/pkg/api"
	"k8s.io/kubernetes/pkg/client/restclient"
	"k8s.io/kubernetes/pkg/client/unversioned"
	"k8s.io/kubernetes/pkg/labels"
	"k8s.io/kubernetes/pkg/util/intstr"
)

var masterHost string

func init() {
	flag.StringVar(&masterHost, "master", "http://127.0.0.1:8080", "TODO: usage")
	flag.Parse()
}

type EtcdCluster struct {
	Kind       string            `json:"kind"`
	ApiVersion string            `json:"apiVersion"`
	Metadata   map[string]string `json:"metadata"`
	Size       int               `json:"size"`
}

type Event struct {
	Type   string
	Object EtcdCluster
}

type etcdClusterController struct {
	kclient *unversioned.Client
}

func (c *etcdClusterController) Run() {
	eventCh, errCh := monitorEtcdCluster()
	for {
		select {
		case event := <-eventCh:
			switch event.Type {
			case "ADDED":
				c.createCluster(event.Object)
			case "DELETED":
				c.deleteCluster(event.Object)
			}
		case err := <-errCh:
			panic(err)
		}
	}
}

func (c *etcdClusterController) createCluster(cluster EtcdCluster) {
	size := cluster.Size
	clusterName := cluster.Metadata["name"]

	initialCluster := []string{}
	for i := 0; i < size; i++ {
		initialCluster = append(initialCluster, fmt.Sprintf("%s-%04d=http://%s-%04d:2380", clusterName, i, clusterName, i))
	}

	for i := 0; i < size; i++ {
		etcdName := fmt.Sprintf("%s-%04d", clusterName, i)

		svc := makeEtcdService(etcdName, clusterName)
		_, err := c.kclient.Services("default").Create(svc)
		if err != nil {
			panic(err)
		}
		// TODO: add and expose client port
		pod := makeEtcdPod(etcdName, clusterName, initialCluster)
		_, err = c.kclient.Pods("default").Create(pod)
		if err != nil {
			panic(err)
		}
	}
}

func (c *etcdClusterController) deleteCluster(cluster EtcdCluster) {
	clusterName := cluster.Metadata["name"]
	option := api.ListOptions{
		LabelSelector: labels.SelectorFromSet(map[string]string{
			"etcd_cluster": clusterName,
		}),
	}

	pods, err := c.kclient.Pods("default").List(option)
	if err != nil {
		panic(err)
	}
	for i := range pods.Items {
		pod := &pods.Items[i]
		err = c.kclient.Pods("default").Delete(pod.Name, nil)
		if err != nil {
			panic(err)
		}
	}

	services, err := c.kclient.Services("default").List(option)
	if err != nil {
		panic(err)
	}
	for i := range services.Items {
		service := &services.Items[i]
		err = c.kclient.Services("default").Delete(service.Name)
		if err != nil {
			panic(err)
		}
	}
}

func monitorEtcdCluster() (<-chan *Event, <-chan error) {
	events := make(chan *Event)
	errc := make(chan error, 1)
	go func() {
		resp, err := http.Get(masterHost + "/apis/coreos.com/v1/namespaces/default/etcdclusters?watch=true")
		if err != nil {
			errc <- err
			return
		}
		if resp.StatusCode != 200 {
			errc <- errors.New("Invalid status code: " + resp.Status)
			return
		}
		log.Println("start watching...")
		for {
			decoder := json.NewDecoder(resp.Body)
			ev := new(Event)
			err = decoder.Decode(ev)
			if err != nil {
				errc <- err
			}
			log.Println("etcd cluster event:", ev.Type, ev.Object.Size, ev.Object.Metadata)
			events <- ev
		}
	}()

	return events, errc
}

func main() {
	c := &etcdClusterController{
		kclient: mustCreateClient(masterHost),
	}
	log.Println("etcd cluster controller starts running...")
	c.Run()
}

func mustCreateClient(host string) *unversioned.Client {
	cfg := &restclient.Config{
		Host:  host,
		QPS:   100,
		Burst: 100,
	}
	c, err := unversioned.New(cfg)
	if err != nil {
		panic(err)
	}
	return c
}

func makeEtcdService(etcdName, clusterName string) *api.Service {
	labels := map[string]string{
		"etcd_node":    etcdName,
		"etcd_cluster": clusterName,
	}
	svc := &api.Service{
		ObjectMeta: api.ObjectMeta{
			Name:   etcdName,
			Labels: labels,
		},
		Spec: api.ServiceSpec{
			Ports: []api.ServicePort{{
				Name:       "server",
				Port:       2380,
				TargetPort: intstr.FromInt(2380),
				Protocol:   api.ProtocolTCP,
			}},
			Selector: labels,
		},
	}
	return svc
}

func makeEtcdPod(etcdName, clusterName string, initialCluster []string) *api.Pod {
	pod := &api.Pod{
		ObjectMeta: api.ObjectMeta{
			Name: etcdName,
			Labels: map[string]string{
				"app":          "etcd",
				"etcd_node":    etcdName,
				"etcd_cluster": clusterName,
			},
		},
		Spec: api.PodSpec{
			Containers: []api.Container{
				{
					Command: []string{
						"/usr/local/bin/etcd",
						"--name",
						etcdName,
						"--initial-advertise-peer-urls",
						fmt.Sprintf("http://%s:2380", etcdName),
						"--listen-peer-urls",
						"http://0.0.0.0:2380",
						"--listen-client-urls",
						"http://0.0.0.0:2379",
						"--advertise-client-urls",
						fmt.Sprintf("http://%s:2379", etcdName),
						"--initial-cluster",
						strings.Join(initialCluster, ","),
						"--initial-cluster-state",
						"new",
					},
					Name:  etcdName,
					Image: "gcr.io/coreos-k8s-scale-testing/etcd-amd64:3.0.4",
					Ports: []api.ContainerPort{
						{
							Name:          "server",
							ContainerPort: int32(2380),
							Protocol:      api.ProtocolTCP,
						},
					},
				},
			},
			RestartPolicy: api.RestartPolicyNever,
		},
	}
	return pod
}
