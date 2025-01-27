// RAINBOND, Application Management Platform
// Copyright (C) 2014-2019 Goodrain Co., Ltd.

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

package cluster

import (
	"context"
	"fmt"
	"net"
	"time"

	"github.com/Sirupsen/logrus"

	"github.com/coreos/etcd/clientv3"

	"github.com/goodrain/rainbond/cmd/gateway/option"
	"github.com/goodrain/rainbond/util"
)

//IPManager ip manager
//Gets all available IP addresses for synchronizing the current node
type IPManager interface {
	//Whether the IP address belongs to the current node
	IPInCurrentHost(net.IP) bool
	Start() error
}

type ipManager struct {
	IPPool  *util.IPPool
	ipLease map[string]clientv3.LeaseID
	etcdCli *clientv3.Client
	config  option.Config
}

//CreateIPManager create ip manage
func CreateIPManager(config option.Config) (IPManager, error) {
	IPPool := util.NewIPPool(config.IgnoreInterface)
	etcdCli, err := clientv3.New(clientv3.Config{
		Endpoints:   config.EtcdEndpoint,
		DialTimeout: time.Duration(config.EtcdTimeout) * time.Second,
	})
	if err != nil {
		return nil, err
	}
	return &ipManager{IPPool: IPPool, config: config, etcdCli: etcdCli, ipLease: make(map[string]clientv3.LeaseID)}, nil
}

//IPInCurrentHost Whether the IP address belongs to the current node
func (i *ipManager) IPInCurrentHost(in net.IP) bool {
	for _, exit := range i.IPPool.GetHostIPs() {
		if exit.Equal(in) {
			return true
		}
	}
	return false
}

func (i *ipManager) Start() error {
	i.IPPool.Ready()
	go i.syncIP()
	return nil
}

func (i *ipManager) syncIP() {
	ips := i.IPPool.GetHostIPs()
	i.updateIP(ips...)
	for ipevent := range i.IPPool.GetWatchIPChan() {
		switch ipevent.Type {
		case util.ADD:
			i.updateIP(ipevent.IP)
		case util.UPDATE:
			i.updateIP(ipevent.IP)
		case util.DEL:
			i.deleteIP(ipevent.IP)
		}
	}
}

func (i *ipManager) updateIP(ips ...net.IP) error {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second*30)
	defer cancel()
	lease := clientv3.NewLease(i.etcdCli)
	for in := range ips {
		ip := ips[in]
		if id, ok := i.ipLease[ip.String()]; ok {
			if _, err := lease.KeepAliveOnce(ctx, id); err == nil {
				continue
			} else {
				logrus.Warningf("keep alive ip key failure %s", err.Error())
			}
		}
		res, err := lease.Grant(ctx, 10)
		if err != nil {
			logrus.Errorf("put gateway ip to etcd failure %s", err.Error())
			return err
		}
		_, err = i.etcdCli.Put(ctx, fmt.Sprintf("/rainbond/gateway/ips/%s", ip.String()), ip.String(), clientv3.WithLease(res.ID))
		if err != nil {
			logrus.Errorf("put gateway ip to etcd failure %s", err.Error())
		}
		logrus.Infof("gateway init ip %s", ip.String())
		i.ipLease[ip.String()] = res.ID
	}
	return nil
}

func (i *ipManager) deleteIP(ips ...net.IP) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second*30)
	defer cancel()
	for _, ip := range ips {
		_, err := i.etcdCli.Delete(ctx, fmt.Sprintf("/rainbond/gateway/ips/%s", ip.String()))
		if err != nil {
			logrus.Errorf("put gateway ip to etcd failure %s", err.Error())
		}
		delete(i.ipLease, ip.String())
	}
}
