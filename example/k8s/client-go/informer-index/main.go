package main

import (
	"fmt"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/cache"
)

func main() {
	stopCh := make(chan struct{})
	block := make(chan struct{})
	defer func() {
		stopCh <- struct{}{}
	}()
	cfg, err := kubernetes.NewForConfig(&rest.Config{
		Host:            "127.0.0.1:61178",
		TLSClientConfig: rest.TLSClientConfig{},
	})
	if err != nil {
		panic(err)
	}
	inf := informers.NewSharedInformerFactory(cfg, 0)
	inf.Core().V1().Pods().Informer().GetIndexer().AddIndexers(cache.Indexers{
		"image": func(obj interface{}) ([]string, error) {
			pod, ok := obj.(*corev1.Pod)
			if !ok {
				return nil, fmt.Errorf("not a pod")
			}
			// 容器中所有的image名称
			images := make([]string, 0, len(pod.Spec.Containers))
			for _, v := range pod.Spec.Containers {
				images = append(images, v.Image)
			}
			return images, nil
		},
	})
	inf.Start(stopCh)
	<-block
}
