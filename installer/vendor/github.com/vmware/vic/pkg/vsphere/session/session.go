// Copyright 2016 VMware, Inc. All Rights Reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//    http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// Package session caches vSphere objects to avoid having to repeatedly
// make govmomi client calls.
//
// To obtain a Session, call Create with a Config. The config
// contains the SDK URL (Service) and the desired vSphere resources.
// Create then connects to Service and stores govmomi objects for
// each corresponding value in Config. The Session is returned and
// the user can use the cached govmomi objects in the exported fields of
// Session instead of directly using a govmomi Client.
//
package session

import (
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"context"

	log "github.com/Sirupsen/logrus"

	"github.com/vmware/govmomi"
	"github.com/vmware/govmomi/find"
	"github.com/vmware/govmomi/object"
	"github.com/vmware/govmomi/session"
	"github.com/vmware/govmomi/vim25"
	"github.com/vmware/govmomi/vim25/methods"
	"github.com/vmware/govmomi/vim25/soap"
	"github.com/vmware/govmomi/vim25/types"
	"github.com/vmware/vic/lib/config"
	"github.com/vmware/vic/pkg/errors"
	"github.com/vmware/vic/pkg/vsphere/extraconfig"
)

const (
	defaultMaxInFlight  = 32
	tlsHandshakeTimeout = 30 * time.Second
)

// Config contains the configuration used to create a Session.
type Config struct {
	// SDK URL or proxy
	Service string
	// Credentials
	User *url.Userinfo
	// Allow insecure connection to Service
	Insecure bool
	// Target thumbprint
	Thumbprint string
	// Keep alive duration
	Keepalive time.Duration
	// User-Agent to identify login sessions (see: govc session.ls)
	UserAgent string

	ClusterPath    string
	DatacenterPath string
	DatastorePath  string
	HostPath       string
	PoolPath       string
}

// Session caches vSphere objects obtained by querying the SDK.
type Session struct {
	*govmomi.Client

	*Config

	Cluster    *object.ComputeResource
	Datacenter *object.Datacenter
	Datastore  *object.Datastore
	Host       *object.HostSystem
	Pool       *object.ResourcePool

	VMFolder *object.Folder

	Finder *find.Finder
}

// RoundTripFunc alias
type RoundTripFunc func(*http.Request) (*http.Response, error)

// RoundTrip method
func (rt RoundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) {
	return rt(r)
}

// LimitConcurrency limits how many requests can be processed at once
func LimitConcurrency(rt http.RoundTripper, limit int) http.RoundTripper {
	limiter := make(chan struct{}, limit)

	return RoundTripFunc(func(r *http.Request) (*http.Response, error) {
		// reserve a slot
		limiter <- struct{}{}

		// free the slot
		defer func() {
			<-limiter
		}()

		// use the given round tripper
		return rt.RoundTrip(r)
	})
}

// NewSession creates a new Session struct. If config is nil,
// it creates a Flags object from the command line arguments or
// environment, and uses that instead to create a Session.
func NewSession(config *Config) *Session {
	log.Debugf("Creating VMOMI session with thumbprint %s", config.Thumbprint)
	return &Session{Config: config}
}

// Vim25 returns the vim25.Client to the caller
func (s *Session) Vim25() *vim25.Client {
	return s.Client.Client
}

// IsVC returns whether the session is backed by VC
func (s *Session) IsVC() bool {
	return s.Client.IsVC()
}

// IsVSAN returns whether the datastore used in the session is backed by VSAN
func (s *Session) IsVSAN(ctx context.Context) bool {
	// #nosec: Errors unhandled.
	dsType, _ := s.Datastore.Type(ctx)

	return dsType == types.HostFileSystemVolumeFileSystemTypeVsan
}

// Create accepts a Config and returns a Session with the cached vSphere resources.
func (s *Session) Create(ctx context.Context) (*Session, error) {
	var vchExtraConfig config.VirtualContainerHostConfigSpec
	source, err := extraconfig.GuestInfoSource()
	if err != nil {
		return nil, err
	}

	extraconfig.Decode(source, &vchExtraConfig)

	s.Service = vchExtraConfig.Target

	s.User = url.UserPassword(vchExtraConfig.Username, vchExtraConfig.Token)

	s.Thumbprint = vchExtraConfig.TargetThumbprint

	_, err = s.Connect(ctx)
	if err != nil {
		return nil, err
	}

	// we're treating this as an atomic behaviour, so log out if we failed
	defer func() {
		if err != nil {
			// #nosec: Errors unhandled.
			s.Client.Logout(ctx)
		}
	}()

	_, err = s.Populate(ctx)
	if err != nil {
		return nil, err
	}

	return s, nil
}

// Connect establishes the connection for the session but nothing more
func (s *Session) Connect(ctx context.Context) (*Session, error) {
	soapURL, err := soap.ParseURL(s.Service)
	if soapURL == nil || err != nil {
		return nil, SDKURLError{
			Service: s.Service,
			Err:     err,
		}
	}

	// Update the service URL with expanded defaults
	s.Service = soapURL.String()

	// VCH components do not include credentials within the target URL
	if s.User != nil {
		soapURL.User = s.User
	}

	soapClient := soap.NewClient(soapURL, s.Insecure)
	soapClient.Version = "6.0" // Pin to 6.0 until we need 6.5+ specific API

	var login func(context.Context) error

	login = func(ctx context.Context) error {
		return s.Client.Login(ctx, soapURL.User)
	}

	soapClient.UserAgent = s.UserAgent

	soapClient.SetThumbprint(soapURL.Host, s.Thumbprint)

	maxInFlight := defaultMaxInFlight
	if e := os.Getenv("VIC_MAX_IN_FLIGHT"); e != "" {
		if i, err := strconv.Atoi(e); err == nil {
			maxInFlight = i
		}
	}
	// Limit the concurrenty of SOAP requests
	if t, ok := soapClient.Transport.(*http.Transport); ok {
		t.MaxIdleConnsPerHost = maxInFlight
		t.TLSHandshakeTimeout = tlsHandshakeTimeout
	}
	soapClient.Transport = LimitConcurrency(soapClient.Transport, maxInFlight)

	// TODO: option to set http.Client.Transport.TLSClientConfig.RootCAs
	vimClient, err := vim25.NewClient(ctx, soapClient)
	if err != nil {
		return nil, SoapClientError{
			Host: soapURL.Host,
			Err:  err,
		}
	}

	if s.Keepalive != 0 {
		vimClient.RoundTripper = session.KeepAliveHandler(soapClient, s.Keepalive,
			func(roundTripper soap.RoundTripper) error {
				_, err := methods.GetCurrentTime(context.Background(), roundTripper)
				if err == nil {
					return nil
				}

				log.Warnf("session keepalive error: %s", err)

				if isNotAuthenticated(err) {

					if err = login(ctx); err != nil {
						log.Errorf("session keepalive failed to re-authenticate: %s", err)
					} else {
						log.Info("session keepalive re-authenticated")
					}
				}

				return nil
			})
	}

	// TODO: get rid of govmomi.Client usage, only provides a few helpers we don't need.
	s.Client = &govmomi.Client{
		Client:         vimClient,
		SessionManager: session.NewManager(vimClient),
	}

	err = login(ctx)
	if err != nil {
		return nil, UserPassLoginError{
			Host: soapURL.Host,
			Err:  err,
		}
	}

	s.Finder = find.NewFinder(s.Vim25(), false)
	// log high-level environment information
	s.logEnvironmentInfo()
	return s, nil
}

// Populate resolves the set of cached resources that should be presented
// This returns accumulated error detail if there is ambiguity, but sets all
// unambiguous or correct resources.
func (s *Session) Populate(ctx context.Context) (*Session, error) {
	// Populate s
	var errs []string
	var err error

	finder := s.Finder

	log.Debug("vSphere resource cache populating...")
	s.Datacenter, err = finder.DatacenterOrDefault(ctx, s.DatacenterPath)
	if err != nil {
		errs = append(errs, fmt.Sprintf("Failure finding dc (%s): %s", s.DatacenterPath, err.Error()))
	} else {
		finder.SetDatacenter(s.Datacenter)
		log.Debugf("Cached dc: %s", s.DatacenterPath)
	}

	finder.SetDatacenter(s.Datacenter)

	s.Cluster, err = finder.ComputeResourceOrDefault(ctx, s.ClusterPath)
	if err != nil {
		errs = append(errs, fmt.Sprintf("Failure finding cluster (%s): %s", s.ClusterPath, err.Error()))
	} else {
		log.Debugf("Cached cluster: %s", s.ClusterPath)
	}

	s.Datastore, err = finder.DatastoreOrDefault(ctx, s.DatastorePath)
	if err != nil {
		errs = append(errs, fmt.Sprintf("Failure finding ds (%s): %s", s.DatastorePath, err.Error()))
	} else {
		log.Debugf("Cached ds: %s", s.DatastorePath)
	}

	s.Host, err = finder.HostSystemOrDefault(ctx, s.HostPath)
	if err != nil {
		if _, ok := err.(*find.DefaultMultipleFoundError); !ok || !s.IsVC() {
			errs = append(errs, fmt.Sprintf("Failure finding host (%s): %s", s.HostPath, err.Error()))
		}
	} else {
		log.Debugf("Cached host: %s", s.HostPath)
	}

	s.Pool, err = finder.ResourcePoolOrDefault(ctx, s.PoolPath)
	if err != nil {
		errs = append(errs, fmt.Sprintf("Failure finding pool (%s): %s", s.PoolPath, err.Error()))
	} else {
		log.Debugf("Cached pool: %s", s.PoolPath)
	}

	if s.Datacenter != nil {
		folders, err := s.Datacenter.Folders(ctx)
		if err != nil {
			errs = append(errs, fmt.Sprintf("Failure finding folders (%s): %s", s.DatacenterPath, err.Error()))
		} else {
			log.Debugf("Cached folders: %s", s.DatacenterPath)
		}
		s.VMFolder = folders.VmFolder
	}

	if len(errs) > 0 {
		log.Debugf("Error count populating vSphere cache: (%d)", len(errs))
		return nil, errors.New(strings.Join(errs, "\n"))
	}
	log.Debug("vSphere resource cache populated...")
	return s, nil
}

func (s *Session) logEnvironmentInfo() {
	a := s.ServiceContent.About
	log.WithFields(log.Fields{
		"Name":        a.Name,
		"Vendor":      a.Vendor,
		"Version":     a.Version,
		"Build":       a.Build,
		"OS Type":     a.OsType,
		"API Type":    a.ApiType,
		"API Version": a.ApiVersion,
		"Product ID":  a.ProductLineId,
		"UUID":        a.InstanceUuid,
	}).Debug("Session Environment Info: ")
	return
}

func isNotAuthenticated(err error) bool {
	if soap.IsSoapFault(err) {
		switch soap.ToSoapFault(err).VimFault().(type) {
		case types.NotAuthenticated:
			return true
		}
	}
	return false
}
