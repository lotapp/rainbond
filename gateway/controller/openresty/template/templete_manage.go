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

package template

import (
	"fmt"
	"io/ioutil"
	"os"
	"os/exec"
	"path"
	"strings"
	"sync"
	text_template "text/template"

	"github.com/goodrain/rainbond/util"

	"github.com/goodrain/rainbond/gateway/controller/openresty/nginxcmd"
	"github.com/pkg/errors"

	"github.com/Sirupsen/logrus"

	"github.com/goodrain/rainbond/cmd/gateway/option"
	"github.com/goodrain/rainbond/gateway/controller/openresty/model"
)

var (
	defBufferSize = 65535
)

//NginxConfigFileTemplete nginx config file manage
//write by templete
type NginxConfigFileTemplete struct {
	templeteFileDirPath string
	configFileDirPath   string
	nginxTmpl           *Template
	serverTmpl          *Template
	tcpUpstreamTmpl     *Template
	writeLocks          map[string]*sync.Mutex
}

//NewNginxConfigFileTemplete new nginx config file manage
func NewNginxConfigFileTemplete() (*NginxConfigFileTemplete, error) {
	var configFileDirPath = "/run/nginx/conf"
	if envConfigFileDirPath := os.Getenv("NGINX_CUSTOM_CONFIG"); envConfigFileDirPath != "" {
		configFileDirPath = envConfigFileDirPath
	}
	var templeteFileDirPath = "/run/nginxtmp/tmpl"
	if envTempleteFileDirPath := os.Getenv("NGINX_CONFIG_TMPL"); envTempleteFileDirPath != "" {
		templeteFileDirPath = envTempleteFileDirPath
	}
	serverTmpl, err := NewTemplate(path.Join(templeteFileDirPath, "servers.tmpl"))
	if err != nil {
		return nil, err
	}
	tcpUpstreamTmpl, err := NewTemplate(path.Join(templeteFileDirPath, "upstreams-tcp.tmpl"))
	if err != nil {
		return nil, err
	}
	nginxTmpl, err := NewTemplate(path.Join(templeteFileDirPath, "nginx.tmpl"))
	if err != nil {
		return nil, err
	}
	return &NginxConfigFileTemplete{
		templeteFileDirPath: templeteFileDirPath,
		configFileDirPath:   configFileDirPath,
		serverTmpl:          serverTmpl,
		tcpUpstreamTmpl:     tcpUpstreamTmpl,
		nginxTmpl:           nginxTmpl,
		writeLocks:          make(map[string]*sync.Mutex),
	}, nil
}

//GetConfigFileDirPath get configfile dir path
func (n *NginxConfigFileTemplete) GetConfigFileDirPath() string {
	return n.configFileDirPath
}

//NewNginxTemplate new nginx main config
func (n *NginxConfigFileTemplete) NewNginxTemplate(data *model.Nginx) error {
	body, err := n.nginxTmpl.Write(data)
	if err != nil {
		return fmt.Errorf("create nginx config by templete failure %s", err.Error())
	}
	nginxConfigFile := path.Join(n.configFileDirPath, "nginx.conf")
	if err := n.writeFile(true, body, nginxConfigFile); err != nil {
		if err == nginxcmd.ErrorCheck {
			return fmt.Errorf("nginx config check error")
		}
		return err
	}
	return nil
}

// WriteServerAndUpstream wriete server
func (n *NginxConfigFileTemplete) WriteServerAndUpstream(first bool, c option.Config, configType, tenant string, server *model.Server, upstream *model.Upstream) error {
	// write a new file just only have one server and and upstream
	if tenant == "" {
		tenant = "default"
	}
	if configType == "" {
		configType = "http"
	}
	if _, ok := n.writeLocks[tenant]; !ok {
		n.writeLocks[tenant] = &sync.Mutex{}
	}
	n.writeLocks[tenant].Lock()
	defer n.writeLocks[tenant].Unlock()
	serverConfigFile := path.Join(n.configFileDirPath, configType, tenant, "servers.conf")
	upstreamConfigFile := path.Join(n.configFileDirPath, "stream", tenant, "upstreams.conf")
	serverBody, err := n.serverTmpl.Write(&NginxServerContext{Server: server, Set: c})
	if err != nil {
		logrus.Errorf("create server config by templete failure %s, ignore it", err.Error())
		return nil
	}
	upstreamBody, err := n.tcpUpstreamTmpl.Write(&NginxUpstreamContext{Upstream: upstream, Set: c})
	if err != nil {
		logrus.Errorf("create upstream config by templete failure %s, ignore it", err.Error())
		return nil
	}
	hasOldServerConfig := false
	hasOldUpstreamConfig := false
	if hasOldUpstreamConfig, err = n.writeFileNotCheck(first, upstreamBody, upstreamConfigFile); err != nil {
		if err == nginxcmd.ErrorCheck {
			logrus.Errorf("upstream config check error")
		} else {
			logrus.Errorf("writer upstream config failure %s", err.Error())
		}
	}

	if hasOldServerConfig, err = n.writeFileNotCheck(first, serverBody, serverConfigFile); err != nil {
		if err == nginxcmd.ErrorCheck {
			logrus.Errorf("server %s config error, will ignore it", func() string {
				if server.ServerName != "" {
					return server.ServerName
				}
				return server.Listen
			}())
		} else {
			logrus.Errorf("writer server config failure %s", err.Error())
		}
	}

	//test
	if err := nginxcmd.CheckConfig(); err != nil {
		//rollback if error
		if hasOldServerConfig {
			if err := os.Rename(serverConfigFile+".bak", serverConfigFile); err != nil {
				logrus.Warningf("rollback config file failure %s", err.Error())
			}
		}
		if hasOldUpstreamConfig {
			if err := os.Rename(upstreamConfigFile+".bak", upstreamConfigFile); err != nil {
				logrus.Warningf("rollback config file failure: %v", err.Error())
			}
		}
		return err
	}
	if hasOldServerConfig {
		if err := os.Remove(serverConfigFile + ".bak"); err != nil {
			logrus.Warningf("remove old config file failure %s", err.Error())
		}
	}
	if hasOldUpstreamConfig {
		if err := os.Remove(upstreamConfigFile + ".bak"); err != nil {
			logrus.Warningf("remove old config file failure %s", err.Error())
		}
	}
	return nil
}

func (n *NginxConfigFileTemplete) writeFileNotCheck(first bool, configBody []byte, configFile string) (hasOldConfig bool, err error) {
	if err := util.CheckAndCreateDir(path.Dir(configFile)); err != nil {
		return false, fmt.Errorf("check or create dir %s failure %s", path.Dir(configFile), err.Error())
	}
	hasOldConfig = true
	//backup
	oldBody, err := ioutil.ReadFile(configFile)
	if err != nil {
		if err != os.ErrNotExist && strings.Contains(err.Error(), "no such file or directory") && !os.IsNotExist(err) {
			logrus.Errorf("read old server config file failure %s", err.Error())
			return false, err
		}
		hasOldConfig = false
	}

	logrus.Debugf("has old config : %v", hasOldConfig)
	logrus.Debugf("old config : %v", string(oldBody))

	if hasOldConfig {
		if err := os.Rename(configFile, configFile+".bak"); err != nil {
			logrus.Errorf("rename server config file failure %s", err.Error())
			return false, err
		}
		//write new body
		if oldBody != nil && !first {
			configBody = append(oldBody, configBody...)
			configBody = append(configBody, []byte("\n")...)
		}
	}

	logrus.Debugf("configBody is : %s", string(configBody))

	cfile, err := os.OpenFile(configFile, os.O_CREATE|os.O_APPEND|os.O_RDWR, 0755)
	if err != nil {
		logrus.Errorf("open server config file failure %s", err.Error())
		return hasOldConfig, err
	}
	defer cfile.Close()
	c, err := cfile.Write(configBody)
	if c < len(configBody) {
		_, err = cfile.Write(configBody[c:])
	}

	return hasOldConfig, err
}

//WriteServer write server config
func (n *NginxConfigFileTemplete) WriteServer(c option.Config, configtype, tenant string, servers ...*model.Server) error {
	if tenant == "" {
		tenant = "default"
	}
	if configtype == "" {
		configtype = "http"
	}
	if _, ok := n.writeLocks[tenant]; !ok {
		n.writeLocks[tenant] = &sync.Mutex{}
	}
	n.writeLocks[tenant].Lock()
	defer n.writeLocks[tenant].Unlock()
	filename := fmt.Sprintf("%s_servers.conf", tenant)
	serverConfigFile := path.Join(n.configFileDirPath, configtype, tenant, filename)
	first := true
	if servers == nil || len(servers) < 1 {
		logrus.Warnf("%s proxy is empty, nginx server[%s] will clean up", tenant, serverConfigFile)
		return n.writeFile(first, []byte{}, serverConfigFile)
	}
	for i, server := range servers {
		body, err := n.serverTmpl.Write(&NginxServerContext{
			Server: servers[i],
			Set:    c,
		})
		if err != nil {
			logrus.Errorf("create server config by templete failure %s", err.Error())
			continue
		}
		if err := n.writeFile(first, body, serverConfigFile); err != nil {
			if err == nginxcmd.ErrorCheck {
				logrus.Errorf("server %s config error, will ignore it", func() string {
					if server.ServerName != "" {
						return server.ServerName
					}
					return server.Listen
				}())
			} else {
				logrus.Errorf("writer server config failure %s", err.Error())
			}
		} else {
			first = false
		}
	}
	return nil
}
func (n *NginxConfigFileTemplete) writeFile(first bool, configBody []byte, configFile string) error {
	if err := util.CheckAndCreateDir(path.Dir(configFile)); err != nil {
		return fmt.Errorf("check or create dir %s failure %s", path.Dir(configFile), err.Error())
	}
	//backup
	noOldConfig := false
	oldBody, err := ioutil.ReadFile(configFile)
	if err != nil {
		if err != os.ErrNotExist && strings.Contains(err.Error(), "no such file or directory") && !os.IsNotExist(err) {
			logrus.Errorf("read old server config file failure %s", err.Error())
			return err
		}
		noOldConfig = true
	}
	newbody := configBody
	if !noOldConfig {
		if err := os.Rename(configFile, configFile+".bak"); err != nil {
			logrus.Errorf("rename server config file failure %s", err.Error())
			return err
		}
		//write new body
		if oldBody != nil && !first {
			configBody = append(oldBody, configBody...)
			configBody = append(configBody, []byte("\n")...)
		}
	}
	cfile, err := os.OpenFile(configFile, os.O_CREATE|os.O_APPEND|os.O_RDWR, 0755)
	if err != nil {
		logrus.Errorf("open server config file failure %s", err.Error())
		return err
	}
	defer cfile.Close()
	c, err := cfile.Write(configBody)
	if c < len(configBody) {
		_, err = cfile.Write(configBody[c:])
	}
	if err != nil {
		logrus.Errorf("write server config file failure %s", err.Error())
		return err
	}
	//test
	if err := nginxcmd.CheckConfig(); err != nil {
		//rollback if error
		if !noOldConfig {
			if err := os.Rename(configFile+".bak", configFile); err != nil {
				logrus.Warningf("rollback config file failre %s", err.Error())
			}
		}
		fmt.Println("failure config body:")
		fmt.Println(string(newbody))
		return err
	}
	//success
	if !noOldConfig {
		if err := os.Remove(configFile + ".bak"); err != nil {
			logrus.Warningf("remove old config file failre %s", err.Error())
		}
	}
	return nil
}

//ClearByTenant clear tenant config
func (n *NginxConfigFileTemplete) ClearByTenant(tenant string) error {
	tenantConfigFile := path.Join(n.configFileDirPath, "http", tenant)
	if err := os.RemoveAll(tenantConfigFile); err != nil {
		return err
	}
	tenantStreamConfigFile := path.Join(n.configFileDirPath, "stream", tenant)
	return os.RemoveAll(tenantStreamConfigFile)
}

//WriteUpstream write upstream config
func (n *NginxConfigFileTemplete) WriteUpstream(set option.Config, tenant string, upstrems ...*model.Upstream) error {
	if tenant == "" {
		tenant = "default"
	}
	if _, ok := n.writeLocks[tenant]; !ok {
		n.writeLocks[tenant] = &sync.Mutex{}
	}
	n.writeLocks[tenant].Lock()
	defer n.writeLocks[tenant].Unlock()
	upstreamConfigFile := path.Join(n.configFileDirPath, "stream", tenant, "upstreams.conf")
	var allBody []byte
	for i := range upstrems {
		body, err := n.tcpUpstreamTmpl.Write(&NginxUpstreamContext{
			Upstream: upstrems[i],
			Set:      set,
		})
		if err != nil {
			logrus.Errorf("create upstream config by templete failure %s", err.Error())
			continue
		}
		allBody = append(allBody, body...)
		allBody = append(allBody, '\n')
	}
	if err := n.writeFile(true, allBody, upstreamConfigFile); err != nil {
		if err == nginxcmd.ErrorCheck {
			logrus.Errorf("upstream config check error")
		} else {
			logrus.Errorf("writer upstream config failure %s", err.Error())
		}
	}
	return nil
}

//NginxServerContext nginx server config
type NginxServerContext struct {
	Server *model.Server
	Set    option.Config
}

//NginxUpstreamContext nginx upstream config
type NginxUpstreamContext struct {
	Upstream *model.Upstream
	Set      option.Config
}

// Template ...
type Template struct {
	tmpl *text_template.Template
	//fw   watch.FileWatcher
	bp *BufferPool
}

//NewTemplate returns a new Template instance or an
//error if the specified template file contains errors
func NewTemplate(fileName string) (*Template, error) {
	tmplFile, err := ioutil.ReadFile(fileName)
	if err != nil {
		return nil, errors.Wrapf(err, "unexpected error reading template %v", tmplFile)
	}
	tmpl, err := text_template.New("gateway").Funcs(funcMap).Parse(string(tmplFile))
	if err != nil {
		return nil, err
	}
	return &Template{
		tmpl: tmpl,
		bp:   NewBufferPool(defBufferSize),
	}, nil
}

func (t *Template) Write(conf interface{}) ([]byte, error) {
	tmplBuf := t.bp.Get()
	defer t.bp.Put(tmplBuf)

	outCmdBuf := t.bp.Get()
	defer t.bp.Put(outCmdBuf)

	if err := t.tmpl.Execute(tmplBuf, conf); err != nil {
		return nil, err
	}
	// squeezes multiple adjacent empty lines to be single
	// spaced this is to avoid the use of regular expressions
	cmd := exec.Command("/run/ingress-controller/clean-nginx-conf.sh")
	cmd.Stdin = tmplBuf
	cmd.Stdout = outCmdBuf
	if err := cmd.Run(); err != nil {
		logrus.Warningf("unexpected error cleaning template: %v", err)
		return tmplBuf.Bytes(), nil
	}
	return outCmdBuf.Bytes(), nil
}
