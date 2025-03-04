// Licensed to Elasticsearch B.V. under one or more contributor
// license agreements. See the NOTICE file distributed with
// this work for additional information regarding copyright
// ownership. Elasticsearch B.V. licenses this file to you under
// the Apache License, Version 2.0 (the "License"); you may
// not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing,
// software distributed under the License is distributed on an
// "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY
// KIND, either express or implied.  See the License for the
// specific language governing permissions and limitations
// under the License.

package kubernetes

import (
	"fmt"
	"time"

	"github.com/gofrs/uuid"
	k8s "k8s.io/client-go/kubernetes"

	"github.com/elastic/beats/v7/libbeat/autodiscover/builder"
	"github.com/elastic/beats/v7/libbeat/common"
	"github.com/elastic/beats/v7/libbeat/common/bus"
	"github.com/elastic/beats/v7/libbeat/common/kubernetes"
	"github.com/elastic/beats/v7/libbeat/common/kubernetes/metadata"
	"github.com/elastic/beats/v7/libbeat/common/safemapstr"
	"github.com/elastic/beats/v7/libbeat/logp"
)

type service struct {
	uuid             uuid.UUID
	config           *Config
	metagen          metadata.MetaGen
	logger           *logp.Logger
	publish          func(bus.Event)
	watcher          kubernetes.Watcher
	namespaceWatcher kubernetes.Watcher
}

// NewServiceEventer creates an eventer that can discover and process service objects
func NewServiceEventer(uuid uuid.UUID, cfg *common.Config, client k8s.Interface, publish func(event bus.Event)) (Eventer, error) {
	logger := logp.NewLogger("autodiscover.service")

	config := defaultConfig()
	err := cfg.Unpack(&config)
	if err != nil {
		return nil, err
	}

	watcher, err := kubernetes.NewWatcher(client, &kubernetes.Service{}, kubernetes.WatchOptions{
		SyncTimeout: config.SyncPeriod,
		Namespace:   config.Namespace,
	}, nil)

	if err != nil {
		return nil, fmt.Errorf("couldn't create watcher for %T due to error %+v", &kubernetes.Service{}, err)
	}

	var namespaceMeta metadata.MetaGen
	var namespaceWatcher kubernetes.Watcher

	metaConf := metadata.GetDefaultResourceMetadataConfig()
	namespaceWatcher, err = kubernetes.NewWatcher(client, &kubernetes.Namespace{}, kubernetes.WatchOptions{
		SyncTimeout: config.SyncPeriod,
		Namespace:   config.Namespace,
	}, nil)
	if err != nil {
		return nil, fmt.Errorf("couldn't create watcher for %T due to error %+v", &kubernetes.Namespace{}, err)
	}

	namespaceMeta = metadata.NewNamespaceMetadataGenerator(metaConf.Namespace, namespaceWatcher.Store())

	p := &service{
		config:           config,
		uuid:             uuid,
		publish:          publish,
		metagen:          metadata.NewServiceMetadataGenerator(cfg, watcher.Store(), namespaceMeta),
		namespaceWatcher: namespaceWatcher,
		logger:           logger,
		watcher:          watcher,
	}

	watcher.AddEventHandler(p)
	return p, nil
}

// OnAdd ensures processing of service objects that are newly created
func (s *service) OnAdd(obj interface{}) {
	s.logger.Debugf("Watcher service add: %+v", obj)
	s.emit(obj.(*kubernetes.Service), "start")
}

// OnUpdate ensures processing of service objects that are updated
func (s *service) OnUpdate(obj interface{}) {
	svc := obj.(*kubernetes.Service)
	// Once service is in terminated state, mark it for deletion
	if svc.GetObjectMeta().GetDeletionTimestamp() != nil {
		time.AfterFunc(s.config.CleanupTimeout, func() { s.emit(svc, "stop") })
	} else {
		s.logger.Debugf("Watcher service update: %+v", obj)
		s.emit(svc, "stop")
		s.emit(svc, "start")
	}
}

// OnDelete ensures processing of service objects that are deleted
func (s *service) OnDelete(obj interface{}) {
	s.logger.Debugf("Watcher service delete: %+v", obj)
	time.AfterFunc(s.config.CleanupTimeout, func() { s.emit(obj.(*kubernetes.Service), "stop") })
}

// GenerateHints creates hints needed for hints builder
func (s *service) GenerateHints(event bus.Event) bus.Event {
	// Try to build a config with enabled builders. Send a provider agnostic payload.
	// Builders are Beat specific.
	e := bus.Event{}
	var kubeMeta common.MapStr

	annotations := make(common.MapStr, 0)
	rawMeta, ok := event["kubernetes"]
	if ok {
		kubeMeta = rawMeta.(common.MapStr)
		// The builder base config can configure any of the field values of kubernetes if need be.
		e["kubernetes"] = kubeMeta
		if rawAnn, ok := kubeMeta["annotations"]; ok {
			anns, _ := rawAnn.(common.MapStr)
			if len(anns) != 0 {
				annotations = anns.Clone()
			}
		}

		// Look at all the namespace level default annotations and do a merge with priority going to the pod annotations.
		if rawNsAnn, ok := kubeMeta["namespace_annotations"]; ok {
			nsAnn, _ := rawNsAnn.(common.MapStr)
			if len(nsAnn) != 0 {
				annotations.DeepUpdateNoOverwrite(nsAnn)
			}
		}
	}
	if host, ok := event["host"]; ok {
		e["host"] = host
	}
	if port, ok := event["port"]; ok {
		e["port"] = port
	}

	hints := builder.GenerateHints(annotations, "", s.config.Prefix)
	s.logger.Debugf("Generated hints %+v", hints)

	if len(hints) != 0 {
		e["hints"] = hints
	}

	s.logger.Debugf("Generated builder event %+v", e)

	return e
}

// Start starts the eventer
func (s *service) Start() error {
	if s.namespaceWatcher != nil {
		if err := s.namespaceWatcher.Start(); err != nil {
			return err
		}
	}
	return s.watcher.Start()
}

// Stop stops the eventer
func (s *service) Stop() {
	s.watcher.Stop()

	if s.namespaceWatcher != nil {
		s.namespaceWatcher.Stop()
	}
}

func (s *service) emit(svc *kubernetes.Service, flag string) {
	host := svc.Spec.ClusterIP

	// If a service doesn't have an IP then dont monitor it
	if host == "" && flag != "stop" {
		return
	}

	eventID := fmt.Sprint(svc.GetObjectMeta().GetUID())
	meta := s.metagen.Generate(svc)

	kubemeta := meta.Clone()
	// Pass annotations to all events so that it can be used in templating and by annotation builders.
	annotations := common.MapStr{}
	for k, v := range svc.GetObjectMeta().GetAnnotations() {
		safemapstr.Put(annotations, k, v)
	}
	kubemeta["annotations"] = annotations

	if s.namespaceWatcher != nil {
		if rawNs, ok, err := s.namespaceWatcher.Store().GetByKey(svc.Namespace); ok && err == nil {
			if namespace, ok := rawNs.(*kubernetes.Namespace); ok {
				nsAnns := common.MapStr{}

				for k, v := range namespace.GetAnnotations() {
					safemapstr.Put(nsAnns, k, v)
				}
				kubemeta["namespace_annotations"] = nsAnns
			}
		}
	}

	for _, port := range svc.Spec.Ports {
		event := bus.Event{
			"provider":   s.uuid,
			"id":         eventID,
			flag:         true,
			"host":       host,
			"port":       int(port.Port),
			"kubernetes": kubemeta,
			"meta": common.MapStr{
				"kubernetes": meta,
			},
		}
		s.publish(event)
	}

}
