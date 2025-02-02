/*
Copyright 2016 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package dns

import (
	"fmt"
	"io/ioutil"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/miekg/dns"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	etcd "go.etcd.io/etcd/client/v2"
	skymsg "k8s.io/dns/third_party/forked/skydns/msg"
	skyserver "k8s.io/dns/third_party/forked/skydns/server"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/tools/cache"

	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/dns/pkg/dns/config"
	"k8s.io/dns/pkg/dns/treecache"
	"k8s.io/dns/pkg/dns/util"
)

const (
	testDomain       = "cluster.local."
	testService      = "testservice"
	testNamespace    = "default"
	testExternalName = "foo.bar.example.com"
)

func newKubeDNS() *KubeDNS {
	return &KubeDNS{
		domain:     testDomain,
		domainPath: util.ReverseArray(strings.Split(strings.TrimRight(testDomain, "."), ".")),

		endpointsStore: cache.NewStore(cache.MetaNamespaceKeyFunc),
		servicesStore:  cache.NewStore(cache.MetaNamespaceKeyFunc),
		nodesStore:     cache.NewStore(cache.MetaNamespaceKeyFunc),

		cache:               treecache.NewTreeCache(),
		reverseRecordMap:    make(map[string]*skymsg.Service),
		clusterIPServiceMap: make(map[string]*v1.Service),
		cacheLock:           sync.RWMutex{},

		config:     config.NewDefaultConfig(),
		configLock: sync.RWMutex{},
		configSync: config.NewNopSync(config.NewDefaultConfig()),
	}
}

func TestPodDns(t *testing.T) {
	const (
		testPodIP      = "1.2.3.4"
		sanitizedPodIP = "1-2-3-4"
	)
	kd := newKubeDNS()

	records, err := kd.Records(sanitizedPodIP+".default.pod."+kd.domain, false)
	require.NoError(t, err)
	assert.Equal(t, 1, len(records))
	assert.Equal(t, testPodIP, records[0].Host)
}

func TestUnnamedSinglePortService(t *testing.T) {
	tests := []struct {
		name            string
		makeServiceFunc func() *v1.Service

		expectedIPs []string
	}{
		{
			name: "ClusterIP IPv4",
			makeServiceFunc: func() *v1.Service {
				return newService(testNamespace, testService, "1.2.3.4", "", 80)
			},
			expectedIPs: []string{"1.2.3.4"},
		},
		{
			name: "ClusterIP IPv6",
			makeServiceFunc: func() *v1.Service {
				return newService(testNamespace, testService, "2001:db8::8a2e:370:7334", "", 80)
			},
			expectedIPs: []string{"2001:db8::8a2e:370:7334"},
		},
		{
			name: "ClusterIPs IPv4/IPv6",
			makeServiceFunc: func() *v1.Service {
				s := newService(testNamespace, testService, "1.2.3.4", "", 80)
				s.Spec.ClusterIPs = []string{"1.2.3.4", "2001:db8::8a2e:370:7334"}
				return s
			},
			expectedIPs: []string{"1.2.3.4", "2001:db8::8a2e:370:7334"},
		},
		{
			name: "ClusterIPs IPv6/IPv4",
			makeServiceFunc: func() *v1.Service {
				s := newService(testNamespace, testService, "2001:db8::8a2e:370:7334", "", 80)
				s.Spec.ClusterIPs = []string{"2001:db8::8a2e:370:7334", "1.2.3.4"}
				return s
			},
			expectedIPs: []string{"2001:db8::8a2e:370:7334", "1.2.3.4"},
		},
	}
	for _, tt := range tests {
		kd := newKubeDNS()

		s := tt.makeServiceFunc()

		// Add the service
		kd.newService(s)
		assertDNSForClusterIP(t, tt.name, kd, s, tt.expectedIPs)
		assertReverseRecord(t, tt.name, kd, s)
		// Delete the service
		kd.removeService(s)
		assertNoDNSForClusterIP(t, kd, s)
		assertNoReverseRecord(t, tt.name, kd, s)
	}
}

func TestNamedSinglePortService(t *testing.T) {
	const (
		portName1 = "http1"
		portName2 = "http2"
	)

	tests := []struct {
		name            string
		makeServiceFunc func() *v1.Service

		expectedIPs []string
	}{
		{
			name: "ClusterIP IPv4",
			makeServiceFunc: func() *v1.Service {
				return newService(testNamespace, testService, "1.2.3.4", portName1, 80)
			},
			expectedIPs: []string{"1.2.3.4"},
		},
		{
			name: "ClusterIP IPv6",
			makeServiceFunc: func() *v1.Service {
				return newService(testNamespace, testService, "2001:db8::8a2e:370:7334", portName1, 80)
			},
			expectedIPs: []string{"2001:db8::8a2e:370:7334"},
		},
		{
			name: "ClusterIPs IPv4/IPv6",
			makeServiceFunc: func() *v1.Service {
				s := newService(testNamespace, testService, "1.2.3.4", portName1, 80)
				s.Spec.ClusterIPs = []string{"1.2.3.4", "2001:db8::8a2e:370:7334"}
				return s
			},
			expectedIPs: []string{"1.2.3.4", "2001:db8::8a2e:370:7334"},
		},
		{
			name: "ClusterIPs IPv6/IPv4",
			makeServiceFunc: func() *v1.Service {
				s := newService(testNamespace, testService, "2001:db8::8a2e:370:7334", portName1, 80)
				s.Spec.ClusterIPs = []string{"2001:db8::8a2e:370:7334", "1.2.3.4"}
				return s
			},
			expectedIPs: []string{"2001:db8::8a2e:370:7334", "1.2.3.4"},
		},
	}

	for _, tt := range tests {
		kd := newKubeDNS()

		s := tt.makeServiceFunc()

		// Add the service
		kd.newService(s)
		assertDNSForClusterIP(t, tt.name, kd, s, tt.expectedIPs)
		assertSRVForNamedPort(t, tt.name, kd, s, portName1, len(tt.expectedIPs))

		newService := *s
		// update the portName of the service
		newService.Spec.Ports[0].Name = portName2
		kd.updateService(s, &newService)
		assertDNSForClusterIP(t, tt.name, kd, s, tt.expectedIPs)
		assertSRVForNamedPort(t, tt.name, kd, s, portName2, len(tt.expectedIPs))
		assertNoSRVForNamedPort(t, kd, s, portName1)

		// Delete the service
		kd.removeService(s)
		assertNoDNSForClusterIP(t, kd, s)
		assertNoSRVForNamedPort(t, kd, s, portName1)
		assertNoSRVForNamedPort(t, kd, s, portName2)
	}
}

func assertARecordsMatchIPs(t *testing.T, records []dns.RR, ips ...string) {
	expectedEndpoints := sets.NewString(ips...)
	gotEndpoints := sets.NewString()
	for _, r := range records {
		if a, ok := r.(*dns.A); !ok {
			t.Errorf("Expected A record, got %#v", a)
		} else {
			gotEndpoints.Insert(a.A.String())
		}
	}
	if !gotEndpoints.Equal(expectedEndpoints) {
		t.Errorf("Expected %v got %v", expectedEndpoints, gotEndpoints)
	}
}

func assertSRVRecordsMatchTarget(t *testing.T, records []dns.RR, targets ...string) {
	expectedTargets := sets.NewString(targets...)
	gotTargets := sets.NewString()
	for _, r := range records {
		if srv, ok := r.(*dns.SRV); !ok {
			t.Errorf("Expected SRV record, got %+v", srv)
		} else {
			gotTargets.Insert(srv.Target)
		}
	}
	if !gotTargets.Equal(expectedTargets) {
		t.Errorf("Expected %v got %v", expectedTargets, gotTargets)
	}
}

func assertSRVRecordsMatchPort(t *testing.T, records []dns.RR, port ...int) {
	expectedPorts := sets.NewInt(port...)
	gotPorts := sets.NewInt()
	for _, r := range records {
		if srv, ok := r.(*dns.SRV); !ok {
			t.Errorf("Expected SRV record, got %+v", srv)
		} else {
			gotPorts.Insert(int(srv.Port))
			t.Logf("got %+v", srv)
		}
	}
	if !gotPorts.Equal(expectedPorts) {
		t.Errorf("Expected %v got %v", expectedPorts, gotPorts)
	}
}

func TestSkySimpleSRVLookup(t *testing.T) {
	kd := newKubeDNS()
	skydnsConfig := &skyserver.Config{Domain: testDomain, DnsAddr: "0.0.0.0:53"}
	skyserver.SetDefaults(skydnsConfig)
	s := skyserver.New(kd, skydnsConfig)

	service := newHeadlessService()
	endpointIPs := []string{"10.0.0.1", "10.0.0.2"}
	endpoints := newEndpoints(service, newSubsetWithOnePort("", 80, endpointIPs...))
	assert.NoError(t, kd.endpointsStore.Add(endpoints))
	kd.newService(service)

	name := strings.Join([]string{testService, testNamespace, "svc", testDomain}, ".")
	question := dns.Question{Name: name, Qtype: dns.TypeSRV, Qclass: dns.ClassINET}

	rec, extra, err := s.SRVRecords(question, name, 512, false)
	if err != nil {
		t.Fatalf("Failed srv record lookup on service with fqdn %v", name)
	}
	assertARecordsMatchIPs(t, extra, endpointIPs...)
	targets := []string{}
	for _, eip := range endpointIPs {
		// A portal service is always created with a port of '0'
		targets = append(targets,
			fmt.Sprintf("%x.%v",
				util.HashServiceRecord(util.NewServiceRecord(eip, 0)), name))
	}
	assertSRVRecordsMatchTarget(t, rec, targets...)
}

func TestSkyPodHostnameSRVLookup(t *testing.T) {
	kd := newKubeDNS()
	skydnsConfig := &skyserver.Config{Domain: testDomain, DnsAddr: "0.0.0.0:53"}
	skyserver.SetDefaults(skydnsConfig)
	s := skyserver.New(kd, skydnsConfig)

	service := newHeadlessService()
	endpointIPs := []string{"10.0.0.1", "10.0.0.2"}
	endpoints := newEndpoints(
		service,
		newSubsetWithOnePortWithHostname("", 80, true, endpointIPs...))
	assert.NoError(t, kd.endpointsStore.Add(endpoints))
	kd.newService(service)
	name := strings.Join([]string{testService, testNamespace, "svc", testDomain}, ".")
	question := dns.Question{Name: name, Qtype: dns.TypeSRV, Qclass: dns.ClassINET}

	rec, _, err := s.SRVRecords(question, name, 512, false)
	if err != nil {
		t.Fatalf("Failed srv record lookup on service with fqdn %v", name)
	}
	targets := []string{}
	for i := range endpointIPs {
		targets = append(targets, fmt.Sprintf("%v.%v", fmt.Sprintf("ep-%d", i), name))
	}
	assertSRVRecordsMatchTarget(t, rec, targets...)
}

func TestSkyNamedPortSRVLookup(t *testing.T) {
	kd := newKubeDNS()
	skydnsConfig := &skyserver.Config{Domain: testDomain, DnsAddr: "0.0.0.0:53"}
	skyserver.SetDefaults(skydnsConfig)
	s := skyserver.New(kd, skydnsConfig)

	service := newHeadlessService()
	eip := "10.0.0.1"
	endpoints := newEndpoints(service, newSubsetWithOnePort("http", 8081, eip))
	assert.NoError(t, kd.endpointsStore.Add(endpoints))
	kd.newService(service)

	name := strings.Join([]string{"_http", "_tcp", testService, testNamespace, "svc", testDomain}, ".")
	question := dns.Question{Name: name, Qtype: dns.TypeSRV, Qclass: dns.ClassINET}
	rec, extra, err := s.SRVRecords(question, name, 512, false)
	if err != nil {
		t.Fatalf("Failed srv record lookup on service with fqdn %v", name)
	}

	svcDomain := strings.Join([]string{testService, testNamespace, "svc", testDomain}, ".")
	assertARecordsMatchIPs(t, extra, eip)
	assertSRVRecordsMatchTarget(
		t, rec, fmt.Sprintf("%x.%v", util.HashServiceRecord(util.NewServiceRecord(eip, 0)), svcDomain))
	assertSRVRecordsMatchPort(t, rec, 8081)
}

func TestSimpleExternalService(t *testing.T) {
	kd := newKubeDNS()
	s := newExternalNameService()
	assert.NoError(t, kd.servicesStore.Add(s))

	kd.newService(s)
	assertDNSForExternalService(t, kd, s)
	kd.removeService(s)
	assertNoDNSForExternalService(t, kd, s)
}

func TestSimpleHeadlessService(t *testing.T) {
	kd := newKubeDNS()
	s := newHeadlessService()
	assert.NoError(t, kd.servicesStore.Add(s))
	endpoints := newEndpoints(s, newSubsetWithOnePort("", 80, "10.0.0.1", "10.0.0.2"), newSubsetWithOnePort("", 8080, "10.0.0.3", "10.0.0.4"))

	assert.NoError(t, kd.endpointsStore.Add(endpoints))
	kd.newService(s)
	assertDNSForHeadlessService(t, kd, endpoints)
	assertNoReverseDNSForHeadlessService(t, kd, endpoints)
	kd.removeService(s)
	assertNoDNSForHeadlessService(t, kd, s)
	assertNoReverseDNSForHeadlessService(t, kd, endpoints)
}

func TestHeadlessServiceWithNamedPorts(t *testing.T) {
	kd := newKubeDNS()
	service := newHeadlessService()
	// add service to store
	assert.NoError(t, kd.servicesStore.Add(service))
	endpoints := newEndpoints(service, newSubsetWithTwoPorts("http1", 80, "http2", 81, "10.0.0.1", "10.0.0.2"),
		newSubsetWithOnePort("https", 443, "10.0.0.3", "10.0.0.4"))

	// We expect 10 records. 6 SRV records. 4 POD records.
	// add endpoints
	assert.NoError(t, kd.endpointsStore.Add(endpoints))

	// add service
	kd.newService(service)
	assertDNSForHeadlessService(t, kd, endpoints)
	assertSRVForHeadlessService(t, kd, service, endpoints)

	// reduce endpoints
	endpoints.Subsets = endpoints.Subsets[:1]
	kd.handleEndpointAdd(endpoints)
	// We expect 6 records. 4 SRV records. 2 POD records.
	assertDNSForHeadlessService(t, kd, endpoints)
	assertSRVForHeadlessService(t, kd, service, endpoints)
	assertNoReverseDNSForHeadlessService(t, kd, endpoints)

	kd.removeService(service)
	assertNoDNSForHeadlessService(t, kd, service)
	assertNoReverseDNSForHeadlessService(t, kd, endpoints)
}

func TestHeadlessServiceEndpointsUpdate(t *testing.T) {
	kd := newKubeDNS()
	service := newHeadlessService()
	// add service to store
	assert.NoError(t, kd.servicesStore.Add(service))

	endpoints := newEndpoints(service, newSubsetWithOnePort("", 80, "10.0.0.1", "10.0.0.2"))
	// add endpoints to store
	assert.NoError(t, kd.endpointsStore.Add(endpoints))

	// add service
	kd.newService(service)
	assertDNSForHeadlessService(t, kd, endpoints)

	// increase endpoints
	endpoints.Subsets = append(endpoints.Subsets,
		newSubsetWithOnePort("", 8080, "10.0.0.3", "10.0.0.4"),
	)
	// expected DNSRecords = 4
	kd.handleEndpointAdd(endpoints)
	assertDNSForHeadlessService(t, kd, endpoints)
	assertNoReverseDNSForHeadlessService(t, kd, endpoints)

	// remove all endpoints
	endpoints.Subsets = []v1.EndpointSubset{}
	kd.handleEndpointAdd(endpoints)
	assertNoDNSForHeadlessService(t, kd, service)
	assertNoReverseDNSForHeadlessService(t, kd, endpoints)

	// remove service
	kd.removeService(service)
	assertNoDNSForHeadlessService(t, kd, service)
	assertNoReverseDNSForHeadlessService(t, kd, endpoints)
}

func TestNamedHeadlessServiceEndpointAdd(t *testing.T) {
	kd := newKubeDNS()

	service := newHeadlessService()
	// add service to store
	assert.NoError(t, kd.servicesStore.Add(service))

	endpoints := newEndpoints(service, v1.EndpointSubset{
		Addresses: []v1.EndpointAddress{
			{
				IP: "10.0.0.1",
				TargetRef: &v1.ObjectReference{
					Kind:      "Pod",
					Name:      "foo",
					Namespace: testNamespace,
				},
				Hostname: "foo",
			},
		},
		Ports: []v1.EndpointPort{},
	})
	// add endpoints to store
	assert.NoError(t, kd.endpointsStore.Add(endpoints))

	// add service
	kd.newService(service)
	assertDNSForHeadlessService(t, kd, endpoints)

	kd.handleEndpointAdd(endpoints)
	assertDNSForHeadlessService(t, kd, endpoints)
	assertReverseDNSForNamedHeadlessService(t, kd, endpoints)
}

func TestNamedHeadlessServiceEndpointUpdate(t *testing.T) {
	kd := newKubeDNS()

	service := newHeadlessService()
	// add service to store
	assert.NoError(t, kd.servicesStore.Add(service))

	oldEndpoints := newEndpoints(service, v1.EndpointSubset{
		Addresses: []v1.EndpointAddress{
			{
				IP: "10.0.0.1",
				TargetRef: &v1.ObjectReference{
					Kind:      "Pod",
					Name:      "foo",
					Namespace: testNamespace,
				},
				Hostname: "foo",
			},
		},
		Ports: []v1.EndpointPort{},
	})
	// add endpoints to store
	assert.NoError(t, kd.endpointsStore.Add(oldEndpoints))

	newEndpoints := newEndpoints(service, v1.EndpointSubset{
		Addresses: []v1.EndpointAddress{
			{
				IP: "10.0.0.2",
				TargetRef: &v1.ObjectReference{
					Kind:      "Pod",
					Name:      "foo",
					Namespace: testNamespace,
				},
				Hostname: "foo",
			},
		},
		Ports: []v1.EndpointPort{},
	})

	// add service
	kd.newService(service)
	assertDNSForHeadlessService(t, kd, oldEndpoints)

	kd.handleEndpointUpdate(oldEndpoints, newEndpoints)
	assertDNSForHeadlessService(t, kd, newEndpoints)
	assertNoReverseDNSForHeadlessService(t, kd, oldEndpoints)
	assertReverseDNSForNamedHeadlessService(t, kd, newEndpoints)
}

func TestNamedHeadlessServiceEndpointDelete(t *testing.T) {
	kd := newKubeDNS()

	service := newHeadlessService()
	// add service to store
	assert.NoError(t, kd.servicesStore.Add(service))

	endpoints := newEndpoints(service, v1.EndpointSubset{
		Addresses: []v1.EndpointAddress{
			{
				IP: "10.0.0.1",
				TargetRef: &v1.ObjectReference{
					Kind:      "Pod",
					Name:      "foo",
					Namespace: testNamespace,
				},
				Hostname: "foo",
			},
		},
		Ports: []v1.EndpointPort{},
	})
	// add endpoints to store
	assert.NoError(t, kd.endpointsStore.Add(endpoints))

	// add service
	kd.newService(service)
	assertDNSForHeadlessService(t, kd, endpoints)

	kd.handleEndpointDelete(endpoints)
	assertDNSForHeadlessService(t, kd, endpoints)
	assertNoReverseDNSForHeadlessService(t, kd, endpoints)
}

func TestHeadlessServiceWithDelayedEndpointsAddition(t *testing.T) {
	kd := newKubeDNS()
	// create service
	service := newHeadlessService()

	// add service to store
	assert.NoError(t, kd.servicesStore.Add(service))

	// add service
	kd.newService(service)
	assertNoDNSForHeadlessService(t, kd, service)

	// create endpoints
	endpoints := newEndpoints(service, newSubsetWithOnePort("", 80, "10.0.0.1", "10.0.0.2"))

	// add endpoints to store
	assert.NoError(t, kd.endpointsStore.Add(endpoints))

	// add endpoints
	kd.handleEndpointAdd(endpoints)

	assertDNSForHeadlessService(t, kd, endpoints)
	assertNoReverseDNSForHeadlessService(t, kd, endpoints)

	// remove service
	kd.removeService(service)
	assertNoDNSForHeadlessService(t, kd, service)
	assertNoReverseDNSForHeadlessService(t, kd, endpoints)
}

// Verifies that a single record with host "a" is returned for query "q".
func verifyRecord(t *testing.T, testCase string, q, a string, kd *KubeDNS) {
	records, err := kd.Records(q, false)
	require.NoError(t, err, testCase)
	assert.Equal(t, 1, len(records), testCase)
	assert.Equal(t, a, records[0].Host, testCase)
}

const federatedServiceFQDN = "testservice.default.myfederation.svc.testcontinent-testreg-testzone.testcontinent-testreg.example.com."

// Verifies that querying KubeDNS for a headless federation service
// returns the DNS hostname when a local service does not exist and
// returns the endpoint IP when a local service exists.
func TestFederationHeadlessService(t *testing.T) {
	kd := newKubeDNS()
	kd.config.Federations = map[string]string{
		"myfederation": "example.com",
	}
	kd.kubeClient = fake.NewSimpleClientset(newNodes())

	// Verify that querying for federation service returns a federation domain name.
	verifyRecord(t, "", "testservice.default.myfederation.svc.cluster.local.",
		federatedServiceFQDN, kd)

	// Add a local service without any endpoint.
	s := newHeadlessService()
	assert.NoError(t, kd.servicesStore.Add(s))
	kd.newService(s)

	// Verify that querying for federation service still returns the federation domain name.
	verifyRecord(t, "", getFederationServiceFQDN(kd, s, "myfederation"),
		federatedServiceFQDN, kd)

	// Now add an endpoint.
	endpoints := newEndpoints(s, newSubsetWithOnePort("", 80, "10.0.0.1"))
	assert.NoError(t, kd.endpointsStore.Add(endpoints))
	kd.updateService(s, s)

	// Verify that querying for federation service returns the local service domain name this time.
	verifyRecord(t, "", getFederationServiceFQDN(kd, s, "myfederation"),
		"testservice.default.svc.cluster.local.", kd)

	// Delete the endpoint.
	endpoints.Subsets = []v1.EndpointSubset{}
	kd.handleEndpointAdd(endpoints)
	kd.updateService(s, s)

	// Verify that querying for federation service returns the federation domain name again.
	verifyRecord(t, "", getFederationServiceFQDN(kd, s, "myfederation"),
		federatedServiceFQDN, kd)
}

// Verifies that querying KubeDNS for a federation service returns the
// DNS hostname if no endpoint exists and returns the local cluster IP
// if endpoints exist.
func TestFederationService(t *testing.T) {
	tests := []struct {
		name            string
		makeServiceFunc func() *v1.Service

		expectedIPs []string
	}{
		{
			name: "ClusterIP IPv4",
			makeServiceFunc: func() *v1.Service {
				return newService(testNamespace, testService, "1.2.3.4", "", 80)
			},
			expectedIPs: []string{"1.2.3.4"},
		},
		{
			name: "ClusterIP IPv6",
			makeServiceFunc: func() *v1.Service {
				return newService(testNamespace, testService, "2001:db8::8a2e:370:7334", "", 80)
			},
			expectedIPs: []string{"2001:db8::8a2e:370:7334"},
		},
		{
			name: "ClusterIPs IPv4/IPv6",
			makeServiceFunc: func() *v1.Service {
				s := newService(testNamespace, testService, "1.2.3.4", "", 80)
				s.Spec.ClusterIPs = []string{"1.2.3.4", "2001:db8::8a2e:370:7334"}
				return s
			},
			expectedIPs: []string{"1.2.3.4", "2001:db8::8a2e:370:7334"},
		},
		{
			name: "ClusterIPs IPv6/IPv4",
			makeServiceFunc: func() *v1.Service {
				s := newService(testNamespace, testService, "2001:db8::8a2e:370:7334", "", 80)
				s.Spec.ClusterIPs = []string{"2001:db8::8a2e:370:7334", "1.2.3.4"}
				return s
			},
			expectedIPs: []string{"2001:db8::8a2e:370:7334", "1.2.3.4"},
		},
	}

	for _, tt := range tests {
		kd := newKubeDNS()
		kd.config.Federations = map[string]string{
			"myfederation": "example.com",
		}
		kd.kubeClient = fake.NewSimpleClientset(newNodes())

		// Verify that querying for federation service returns the federation domain name.
		verifyRecord(t, tt.name, "testservice.default.myfederation.svc.cluster.local.",
			federatedServiceFQDN, kd)

		// Add a local service without any endpoint.
		s := tt.makeServiceFunc()
		assert.NoError(t, kd.servicesStore.Add(s), tt.name)
		kd.newService(s)

		// Verify that querying for federation service still returns the federation domain name.
		verifyRecord(t, tt.name, getFederationServiceFQDN(kd, s, "myfederation"),
			federatedServiceFQDN, kd)

		// Now add an endpoint.
		endpoints := newEndpoints(s, newSubsetWithOnePort("", 80, "10.0.0.1"))
		assert.NoError(t, kd.endpointsStore.Add(endpoints), tt.name)
		kd.updateService(s, s)

		// Verify that querying for federation service returns the local service domain name this time.
		verifyRecord(t, tt.name, getFederationServiceFQDN(kd, s, "myfederation"),
			"testservice.default.svc.cluster.local.", kd)

		// Remove the endpoint.
		endpoints.Subsets = []v1.EndpointSubset{}
		kd.handleEndpointAdd(endpoints)
		kd.updateService(s, s)

		// Verify that querying for federation service returns the federation domain name again.
		verifyRecord(t, tt.name, getFederationServiceFQDN(kd, s, "myfederation"),
			federatedServiceFQDN, kd)
	}
}

func TestFederationQueryWithoutCache(t *testing.T) {
	kd := newKubeDNS()
	kd.config.Federations = map[string]string{
		"myfederation":     "example.com",
		"secondfederation": "second.example.com",
	}
	kd.kubeClient = fake.NewSimpleClientset(newNodes())

	testValidFederationQueries(t, kd)
	testInvalidFederationQueries(t, kd)
}

func TestFederationQueryWithCache(t *testing.T) {
	kd := newKubeDNS()
	kd.config.Federations = map[string]string{
		"myfederation":     "example.com",
		"secondfederation": "second.example.com",
	}

	// Add a node to the cache.
	nodeList := newNodes()
	if err := kd.nodesStore.Add(&nodeList.Items[1]); err != nil {
		t.Errorf("failed to add the node to the cache: %v", err)
	}

	testValidFederationQueries(t, kd)
	testInvalidFederationQueries(t, kd)
}

func testValidFederationQueries(t *testing.T, kd *KubeDNS) {
	queries := []struct {
		q string
		a string
	}{
		// Federation suffix is just a domain.
		{
			q: "mysvc.myns.myfederation.svc.cluster.local.",
			a: "mysvc.myns.myfederation.svc.testcontinent-testreg-testzone.testcontinent-testreg.example.com.",
		},
		// Federation suffix is a subdomain.
		{
			q: "secsvc.default.secondfederation.svc.cluster.local.",
			a: "secsvc.default.secondfederation.svc.testcontinent-testreg-testzone.testcontinent-testreg.second.example.com.",
		},
	}

	for _, query := range queries {
		verifyRecord(t, "", query.q, query.a, kd)
	}
}

func testInvalidFederationQueries(t *testing.T, kd *KubeDNS) {
	noAnswerQueries := []string{
		"mysvc.myns.svc.cluster.local.",
		"mysvc.default.nofederation.svc.cluster.local.",
	}
	for _, q := range noAnswerQueries {
		records, err := kd.Records(q, false)
		if err == nil {
			t.Errorf("expected not found error, got nil")
		}
		if etcdErr, ok := err.(etcd.Error); !ok || etcdErr.Code != etcd.ErrorCodeKeyNotFound {
			t.Errorf("expected not found error, got %v", err)
		}
		assert.Equal(t, 0, len(records))
	}
}

func checkConfigEqual(t *testing.T, kd *KubeDNS, expected *config.Config) {
	const timeout = time.Duration(5)

	start := time.Now()

	ok := false

	for time.Since(start) < timeout*time.Second {
		kd.configLock.RLock()
		isEqual := reflect.DeepEqual(expected.Federations, kd.config.Federations)
		kd.configLock.RUnlock()

		if isEqual {
			ok = true
			break
		}
	}

	if !ok {
		t.Errorf("Federations should be %v, got %v",
			expected.Federations, kd.config.Federations)
	}
}

func TestConfigSync(t *testing.T) {
	kd := newKubeDNS()
	mockSync := config.NewMockSync(
		&config.Config{Federations: make(map[string]string)}, nil)
	kd.configSync = mockSync

	kd.startConfigMapSync()

	checkConfigEqual(t, kd, &config.Config{Federations: make(map[string]string)})
	// update
	mockSync.Chan <- &config.Config{Federations: map[string]string{"name1": "domain1"}}
	checkConfigEqual(t, kd, &config.Config{Federations: map[string]string{"name1": "domain1"}})
	// update
	mockSync.Chan <- &config.Config{Federations: map[string]string{"name2": "domain2"}}
	checkConfigEqual(t, kd, &config.Config{Federations: map[string]string{"name2": "domain2"}})
}

func TestConfigSyncInitialMap(t *testing.T) {
	// start with different initial map
	kd := newKubeDNS()
	mockSync := config.NewMockSync(
		&config.Config{Federations: map[string]string{"name3": "domain3"}}, nil)
	kd.configSync = mockSync

	kd.startConfigMapSync()
	checkConfigEqual(t, kd, &config.Config{Federations: map[string]string{"name3": "domain3"}})
}

func TestUpdateConfig(t *testing.T) {
	tmpdir, err := ioutil.TempDir("", "test")
	defaultResolvFile = filepath.Join(tmpdir, "resolv.conf")
	require.NoError(t, err)
	defer os.RemoveAll(tmpdir)

	kd := newKubeDNS()
	kd.SkyDNSConfig = new(skyserver.Config)

	nextConfig := &config.Config{UpstreamNameservers: []string{"badNameserver"}}
	kd.updateConfig(nextConfig)
	assert.NotEqual(t, nextConfig, kd.config)
	assert.Equal(t, []string{}, kd.SkyDNSConfig.Nameservers)

	err = ioutil.WriteFile(defaultResolvFile, []byte("nameserver 127.0.0.1"), 0666)
	require.NoError(t, err)

	kd.updateConfig(nextConfig)
	assert.Equal(t, []string{"127.0.0.1:53"}, kd.SkyDNSConfig.Nameservers)

	nextConfig = &config.Config{UpstreamNameservers: []string{"192.0.2.123:10086", "192.0.2.123"}}
	kd.updateConfig(nextConfig)
	assert.Equal(t, nextConfig, kd.config)
	assert.Equal(t, []string{"192.0.2.123:10086", "192.0.2.123:53"}, kd.SkyDNSConfig.Nameservers)

	nextConfig = new(config.Config)
	kd.updateConfig(nextConfig)
	assert.Equal(t, []string{"127.0.0.1:53"}, kd.SkyDNSConfig.Nameservers)
}

func newNodes() *v1.NodeList {
	return &v1.NodeList{
		Items: []v1.Node{
			// Node without annotation.
			{
				ObjectMeta: metav1.ObjectMeta{
					Name: "testnode-0",
				},
			},
			{
				ObjectMeta: metav1.ObjectMeta{
					Name: "testnode-1",
					Labels: map[string]string{
						// Note: The zone name here is an arbitrary string and doesn't exactly follow the
						// format used by the cloud providers to name their zones. But that shouldn't matter
						// for these tests here.
						v1.LabelZoneFailureDomain: "testcontinent-testreg-testzone",
						v1.LabelZoneRegion:        "testcontinent-testreg",
					},
				},
			},
		},
	}
}

func newService(namespace, serviceName, clusterIP, portName string, portNumber int32) *v1.Service {
	service := v1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      serviceName,
			Namespace: namespace,
		},
		Spec: v1.ServiceSpec{
			ClusterIP: clusterIP,
			Ports: []v1.ServicePort{
				{Port: portNumber, Name: portName, Protocol: "TCP"},
			},
		},
	}
	return &service
}

func newExternalNameService() *v1.Service {
	service := v1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      testService,
			Namespace: testNamespace,
		},
		Spec: v1.ServiceSpec{
			ClusterIP:    "None",
			Type:         v1.ServiceTypeExternalName,
			ExternalName: testExternalName,
			Ports: []v1.ServicePort{
				{Port: 0},
			},
		},
	}
	return &service
}

func newHeadlessService() *v1.Service {
	service := v1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      testService,
			Namespace: testNamespace,
		},
		Spec: v1.ServiceSpec{
			ClusterIP: "None",
			Ports: []v1.ServicePort{
				{Port: 0},
			},
		},
	}
	return &service
}

func newEndpoints(service *v1.Service, subsets ...v1.EndpointSubset) *v1.Endpoints {
	endpoints := v1.Endpoints{
		ObjectMeta: service.ObjectMeta,
		Subsets:    []v1.EndpointSubset{},
	}

	endpoints.Subsets = append(endpoints.Subsets, subsets...)
	return &endpoints
}

func newSubsetWithOnePort(portName string, port int32, ips ...string) v1.EndpointSubset {
	return newSubsetWithOnePortWithHostname(portName, port, false, ips...)
}

func newSubsetWithOnePortWithHostname(portName string, port int32, addHostname bool, ips ...string) v1.EndpointSubset {
	subset := newSubset()
	subset.Ports = append(subset.Ports, v1.EndpointPort{Port: port, Name: portName, Protocol: "TCP"})
	for i, ip := range ips {
		var hostname string
		if addHostname {
			hostname = fmt.Sprintf("ep-%d", i)
		}
		subset.Addresses = append(subset.Addresses, v1.EndpointAddress{IP: ip, Hostname: hostname})
	}
	return subset
}

func newSubsetWithTwoPorts(portName1 string, portNumber1 int32, portName2 string, portNumber2 int32, ips ...string) v1.EndpointSubset {
	subset := newSubsetWithOnePort(portName1, portNumber1, ips...)
	subset.Ports = append(subset.Ports, v1.EndpointPort{Port: portNumber2, Name: portName2, Protocol: "TCP"})
	return subset
}

func newSubset() v1.EndpointSubset {
	subset := v1.EndpointSubset{
		Addresses: []v1.EndpointAddress{},
		Ports:     []v1.EndpointPort{},
	}
	return subset
}

func assertSRVForHeadlessService(t *testing.T, kd *KubeDNS, s *v1.Service, e *v1.Endpoints) {
	for _, subset := range e.Subsets {
		for _, port := range subset.Ports {
			records, err := kd.Records(getSRVFQDN(kd, s, port.Name), false)
			require.NoError(t, err)
			assertRecordPortsMatchPort(t, port.Port, records)
			assertCNameRecordsMatchEndpointIPs(t, kd, subset.Addresses, records)
		}
	}
}

func assertDNSForHeadlessService(t *testing.T, kd *KubeDNS, e *v1.Endpoints) {
	records, err := kd.Records(getEndpointsFQDN(kd, e), false)
	require.NoError(t, err)
	endpoints := map[string]bool{}
	for _, subset := range e.Subsets {
		for _, endpointAddress := range subset.Addresses {
			endpoints[endpointAddress.IP] = true
		}
	}
	assert.Equal(t, len(endpoints), len(records))
	for _, record := range records {
		_, found := endpoints[record.Host]
		assert.True(t, found)
	}
}

func assertReverseDNSForNamedHeadlessService(t *testing.T, kd *KubeDNS, e *v1.Endpoints) {
	for _, subset := range e.Subsets {
		for _, endpointAddress := range subset.Addresses {
			record := kd.reverseRecordMap[endpointAddress.IP]
			t.Logf("got reverse host name %s", record.Host)
			assert.Equal(t, record.Host, getPodsFQDN(kd, e, endpointAddress.Hostname))
		}
	}
}

func assertNoReverseDNSForHeadlessService(t *testing.T, kd *KubeDNS, e *v1.Endpoints) {
	for _, subset := range e.Subsets {
		for _, endpointAddress := range subset.Addresses {
			assert.Nil(t, kd.reverseRecordMap[endpointAddress.IP])
		}
	}
}

func assertDNSForExternalService(t *testing.T, kd *KubeDNS, s *v1.Service) {
	records, err := kd.Records(getServiceFQDN(kd.domain, s), false)
	require.NoError(t, err)
	assert.Equal(t, 1, len(records))
	assert.Equal(t, testExternalName, records[0].Host)
}

func assertRecordPortsMatchPort(t *testing.T, port int32, records []skymsg.Service) {
	for _, record := range records {
		assert.Equal(t, port, int32(record.Port))
	}
}

func assertCNameRecordsMatchEndpointIPs(t *testing.T, kd *KubeDNS, e []v1.EndpointAddress, records []skymsg.Service) {
	endpoints := map[string]bool{}
	for _, endpointAddress := range e {
		endpoints[endpointAddress.IP] = true
	}
	assert.Equal(t, len(e), len(records), "unexpected record count")
	for _, record := range records {
		_, found := endpoints[getIPForCName(t, kd, record.Host)]
		assert.True(t, found, "Did not find endpoint with address:%s", record.Host)
	}
}

func getIPForCName(t *testing.T, kd *KubeDNS, cname string) string {
	records, err := kd.Records(cname, false)
	require.NoError(t, err)
	assert.Equal(t, 1, len(records), "Could not get IP for CNAME record for %s", cname)
	assert.NotNil(t, net.ParseIP(records[0].Host), "Invalid IP address %q", records[0].Host)
	return records[0].Host
}

func assertNoDNSForHeadlessService(t *testing.T, kd *KubeDNS, s *v1.Service) {
	records, err := kd.Records(getServiceFQDN(kd.domain, s), false)
	require.Error(t, err)
	assert.Equal(t, 0, len(records))
}

func assertNoDNSForExternalService(t *testing.T, kd *KubeDNS, s *v1.Service) {
	records, err := kd.Records(getServiceFQDN(kd.domain, s), false)
	require.Error(t, err)
	assert.Equal(t, 0, len(records))
}

func assertSRVForNamedPort(t *testing.T, testCase string, kd *KubeDNS, s *v1.Service, portName string, recordsNum int) {
	records, err := kd.Records(getSRVFQDN(kd, s, portName), false)
	require.NoError(t, err, testCase)

	assert.Equal(t, recordsNum, len(records), testCase)

	svcFQDN := getServiceFQDN(kd.domain, s)
	for _, record := range records {
		assert.Equal(t, svcFQDN, record.Host, testCase)
	}
}

func assertNoSRVForNamedPort(t *testing.T, kd *KubeDNS, s *v1.Service, portName string) {
	records, err := kd.Records(getSRVFQDN(kd, s, portName), false)
	require.Error(t, err)
	assert.Equal(t, 0, len(records))
}

func assertNoDNSForClusterIP(t *testing.T, kd *KubeDNS, s *v1.Service) {
	serviceFQDN := getServiceFQDN(kd.domain, s)
	queries := getEquivalentQueries(serviceFQDN, s.Namespace)
	for _, query := range queries {
		records, err := kd.Records(query, false)
		require.Error(t, err)
		assert.Equal(t, 0, len(records))
	}
}

func assertDNSForClusterIP(t *testing.T, testCase string, kd *KubeDNS, s *v1.Service, expectedIPs []string) {
	serviceFQDN := getServiceFQDN(kd.domain, s)
	queries := getEquivalentQueries(serviceFQDN, s.Namespace)
	for _, query := range queries {
		records, err := kd.Records(query, false)
		require.NoError(t, err, testCase)

		hosts := make([]string, 0, len(records))
		for _, record := range records {
			hosts = append(hosts, record.Host)
		}
		assert.ElementsMatch(t, expectedIPs, hosts, testCase)
	}
}

func assertReverseRecord(t *testing.T, testCase string, kd *KubeDNS, s *v1.Service) {
	for _, ip := range util.GetClusterIPs(s) {
		reverseLookup, err := makePTRRecord(ip)
		require.NoError(t, err, testCase)
		reverseRecord, err := kd.ReverseRecord(reverseLookup)
		require.NoError(t, err, testCase)
		assert.Equal(t, getServiceFQDN(kd.domain, s), reverseRecord.Host, testCase)
	}
}

func assertNoReverseRecord(t *testing.T, testCase string, kd *KubeDNS, s *v1.Service) {
	for _, ip := range util.GetClusterIPs(s) {
		reverseLookup, err := makePTRRecord(ip)
		require.NoError(t, err, testCase)
		reverseRecord, err := kd.ReverseRecord(reverseLookup)
		require.Error(t, err)
		require.Nil(t, reverseRecord)
	}
}

// 10.47.32.22 -> 22.32.47.10.in-addr.arpa.
// 4321:0:1:2:3:4:567:89ab -> b.a.9.8.7.6.5.0.4.0.0.0.3.0.0.0.2.0.0.0.1.0.0.0.0.0.0.0.1.2.3.4.ip6.arpa.
func makePTRRecord(ip string) (string, error) {
	if net.ParseIP(ip).To4() != nil {
		segments := util.ReverseArray(strings.Split(ip, "."))
		return fmt.Sprintf("%s%s", strings.Join(segments, "."), util.ArpaSuffix), nil
	}

	const ipv6nibbleCount = 32

	if ipv6 := net.ParseIP(ip).To16(); ipv6 != nil {
		b := make([]string, 0, ipv6nibbleCount)
		for i := 0; i < len(ipv6); i += 2 {
			for _, c := range fmt.Sprintf("%04x", int64(ipv6[i])<<8|int64(ipv6[i+1])) {
				b = append(b, string(c))
			}
		}
		return fmt.Sprintf("%s%s", strings.Join(util.ReverseArray(b), "."), util.ArpaSuffixV6), nil
	}

	return "", fmt.Errorf("incorrect ip adress: %q", ip)
}

func getEquivalentQueries(serviceFQDN, namespace string) []string {
	return []string{
		serviceFQDN,
		strings.Replace(serviceFQDN, ".svc.", ".*.", 1),
		strings.Replace(serviceFQDN, namespace, "*", 1),
		strings.Replace(strings.Replace(serviceFQDN, namespace, "*", 1), ".svc.", ".*.", 1),
		"*." + serviceFQDN,
	}
}

func getFederationServiceFQDN(kd *KubeDNS, s *v1.Service, federationName string) string {
	return fmt.Sprintf("%s.%s.%s.svc.%s", s.Name, s.Namespace, federationName, kd.domain)
}

func getEndpointsFQDN(kd *KubeDNS, e *v1.Endpoints) string {
	return fmt.Sprintf("%s.%s.svc.%s", e.Name, e.Namespace, kd.domain)
}

func getPodsFQDN(kd *KubeDNS, e *v1.Endpoints, podHostName string) string {
	return fmt.Sprintf("%s.%s.%s.svc.%s", podHostName, e.Name, e.Namespace, kd.domain)
}

func getSRVFQDN(kd *KubeDNS, s *v1.Service, portName string) string {
	return fmt.Sprintf("_%s._tcp.%s.%s.svc.%s", portName, s.Name, s.Namespace, kd.domain)
}
