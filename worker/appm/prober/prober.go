// RAINBOND, Application Management Platform
// Copyright (C) 2014-2017 Goodrain Co., Ltd.

// This program is free software: you can redistribute it and/or modify
// it under the terms of the GNU General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version. For any non-GPL usage of Rainbond,
// one or multiple Commercial Licenses authorized by Goodrain Co., Ltd.
// must be obtained first.

// This program is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
// GNU General Public License for more details.

// You should have received a copy of the GNU General Public License
// along with this program. If not, see <http://www.gnu.org/licenses/>.

package prober

import (
	"context"
	"fmt"
	"net"
	"strings"

	"github.com/Sirupsen/logrus"
	"github.com/eapache/channels"
	"github.com/goodrain/rainbond/db"
	"github.com/goodrain/rainbond/db/model"
	uitlprober "github.com/goodrain/rainbond/util/prober"
	v1 "github.com/goodrain/rainbond/util/prober/types/v1"
	"github.com/goodrain/rainbond/worker/appm/store"
	"github.com/goodrain/rainbond/worker/appm/thirdparty/discovery"
	appmv1 "github.com/goodrain/rainbond/worker/appm/types/v1"
	corev1 "k8s.io/api/core/v1"
)

// Prober is the interface that wraps the required methods to maintain status
// about upstream servers(Endpoints) associated with a third-party service.
type Prober interface {
	Start()
	Stop()
	UpdateProbes(info []*store.ProbeInfo)
	StopProbe(uuids []string)
}

// NewProber creates a new third-party service prober.
func NewProber(store store.Storer,
	probeCh *channels.RingChannel,
	updateCh *channels.RingChannel) Prober {
	ctx, cancel := context.WithCancel(context.Background())
	return &tpProbe{
		utilprober: uitlprober.NewProber(ctx, cancel),
		dbm:        db.GetManager(),
		store:      store,

		updateCh: updateCh,
		probeCh:  probeCh,

		ctx:    ctx,
		cancel: cancel,
	}
}

// third-party service probe
type tpProbe struct {
	utilprober uitlprober.Prober
	dbm        db.Manager
	store      store.Storer

	probeCh  *channels.RingChannel
	updateCh *channels.RingChannel

	ctx    context.Context
	cancel context.CancelFunc
}

func createService(probe *model.TenantServiceProbe) *v1.Service {
	return &v1.Service{
		Disable: false,
		ServiceHealth: &v1.Health{
			Model:        probe.Scheme,
			TimeInterval: probe.PeriodSecond,
			MaxErrorsNum: probe.FailureThreshold,
		},
	}
}

func (t *tpProbe) Start() {
	t.utilprober.Start()

	go func() {
		for {
			select {
			case event := <-t.probeCh.Out():
				if event == nil {
					return
				}
				evt := event.(store.Event)
				switch evt.Type {
				case store.CreateEvent:
					infos := evt.Obj.([]*store.ProbeInfo)
					t.UpdateProbes(infos)
				case store.UpdateEvent:
					infos := evt.Obj.([]*store.ProbeInfo)
					t.UpdateProbes(infos)
				case store.DeleteEvent:
					uuids := evt.Obj.([]string)
					t.StopProbe(uuids)
				}
			case <-t.ctx.Done():
				return
			}
		}
	}()
}

// Stop stops prober.
func (t *tpProbe) Stop() {
	t.cancel()
}

func (t *tpProbe) UpdateProbes(infos []*store.ProbeInfo) {
	var services []*v1.Service
	for _, info := range infos {
		service, probeInfo := t.createServices(info)
		if service == nil {
			logrus.Debugf("Empty service, stop creating probe")
			continue
		}
		services = append(services, service)
		// watch
		if t.utilprober.CheckIfExist(service) {
			continue
		}
		go func(service *v1.Service, info *store.ProbeInfo) {
			watcher := t.utilprober.WatchServiceHealthy(service.Name)
			t.utilprober.EnableWatcher(watcher.GetServiceName(), watcher.GetID())
			defer watcher.Close()
			defer t.utilprober.DisableWatcher(watcher.GetServiceName(), watcher.GetID())
			for {
				select {
				case event := <-watcher.Watch():
					if event == nil {
						return
					}
					switch event.Status {
					case v1.StatHealthy:
						obj := &appmv1.RbdEndpoint{
							UUID: info.UUID,
							IP:   info.IP,
							Port: int(info.Port),
							Sid:  info.Sid,
						}
						t.updateCh.In() <- discovery.Event{
							Type: discovery.HealthEvent,
							Obj:  obj,
						}
					case v1.StatDeath, v1.StatUnhealthy:
						if event.ErrorNumber > service.ServiceHealth.MaxErrorsNum {
							if probeInfo.Mode == model.OfflineFailureAction.String() {
								obj := &appmv1.RbdEndpoint{
									UUID: info.UUID,
									IP:   info.IP,
									Port: int(info.Port),
									Sid:  info.Sid,
								}
								t.updateCh.In() <- discovery.Event{
									Type: discovery.DeleteEvent,
									Obj:  obj,
								}
							} else {
								obj := &appmv1.RbdEndpoint{
									UUID: info.UUID,
									IP:   info.IP,
									Port: int(info.Port),
									Sid:  info.Sid,
								}
								t.updateCh.In() <- discovery.Event{
									Type: discovery.UnhealthyEvent,
									Obj:  obj,
								}
							}
						}
					}
				case <-t.ctx.Done():
					// TODO: should stop for one service, not all services.
					return
				}
			}
		}(service, info)
	}
	t.utilprober.UpdateServicesProbe(services)
}

func (t *tpProbe) StopProbe(uuids []string) {
	for _, name := range uuids {
		t.utilprober.StopProbes([]string{name})
	}
}

// GetProbeInfo returns probe info associated with sid.
// If there is a probe in the database, return directly
// If there is no probe in the database, return a default probe
func (t *tpProbe) GetProbeInfo(sid string) (*model.TenantServiceProbe, error) {
	probes, err := t.dbm.ServiceProbeDao().GetServiceProbes(sid)
	if err != nil || probes == nil || len(probes) == 0 || *(probes[0].IsUsed) == 0 {
		if err != nil {
			logrus.Warningf("ServiceID: %s; error getting probes: %v", sid, err)
		}
		// no defined probe, use default one
		return &model.TenantServiceProbe{
			Scheme:           "tcp",
			PeriodSecond:     5,
			FailureThreshold: 3,
			FailureAction:    model.IgnoreFailureAction.String(),
		}, nil
	}
	return probes[0], nil
}

func (t *tpProbe) createServices(probeInfo *store.ProbeInfo) (*v1.Service, *model.TenantServiceProbe) {
	if probeInfo.IP == "8.8.8.8" {
		app := t.store.GetAppService(probeInfo.Sid)
		if len(app.GetServices()) >= 1 {
			appService := app.GetServices()[0]
			if appService.Annotations != nil && appService.Annotations["domain"] != "" {
				probeInfo.IP = appService.Annotations["domain"]
				logrus.Debugf("domain address is : %s", probeInfo.IP)
			}
		}
		if probeInfo.IP == "8.8.8.8" {
			logrus.Warningf("serviceID: %s, is a domain thirdpart endpoint, but do not found domain info", probeInfo.Sid)
			return nil, nil
		}
	}
	logrus.Debugf("create probe[sid: %s, address: %s, port: %d]", probeInfo.Sid, probeInfo.IP, probeInfo.Port)
	tsp, err := t.GetProbeInfo(probeInfo.Sid)
	if err != nil {
		logrus.Warningf("ServiceID: %s; Unexpected error occurred, ignore the creation of "+
			"probes: %s", probeInfo.Sid, err.Error())
		return nil, nil
	}
	if tsp.Mode == "liveness" {
		tsp.Mode = model.IgnoreFailureAction.String()
	}
	service := createService(tsp)
	service.Sid = probeInfo.Sid
	service.Name = probeInfo.UUID
	service.ServiceHealth.Port = int(probeInfo.Port)
	service.ServiceHealth.Name = service.Name
	address := fmt.Sprintf("%s:%d", probeInfo.IP, probeInfo.Port)
	if service.ServiceHealth.Model == "tcp" {
		address = parseTCPHostAddress(probeInfo.IP, probeInfo.Port)
	}
	service.ServiceHealth.Address = address
	return service, tsp
}

func (t *tpProbe) createServiceNames(ep *corev1.Endpoints) string {
	return ep.GetLabels()["uuid"]
}

func parseTCPHostAddress(address string, port int32) string {
	logrus.Debugf("tcp probe address=%s, port=%d", address, port)
	if strings.HasPrefix(address, "https://") {
		address = strings.Split(address, "https://")[1]
	}
	if strings.HasPrefix(address, "http://") {
		address = strings.Split(address, "http://")[1]
	}
	if strings.Contains(address, ":") {
		address = strings.Split(address, ":")[0]
	}

	ns, err := net.LookupHost(address)
	if err != nil || len(ns) == 0 {
		return address
	}

	address = ns[0]

	address = fmt.Sprintf("%s:%d", address, port)
	logrus.Debugf("parse tcp probe address = %s", address)
	return address
}
