/*
 * Licensed to the Apache Software Foundation (ASF) under one or more
 * contributor license agreements.  See the NOTICE file distributed with
 * this work for additional information regarding copyright ownership.
 * The ASF licenses this file to You under the Apache License, Version 2.0
 * (the "License"); you may not use this file except in compliance with
 * the License.  You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

package cluster

import (
	"math/rand"
	"sync"
	"sync/atomic"
	"time"

	"mosn.io/api"
	"mosn.io/mosn/pkg/types"
)

// NewLoadBalancer can be register self defined type
var lbFactories map[types.LoadBalancerType]func(types.HostSet) types.LoadBalancer

func RegisterLBType(lbType types.LoadBalancerType, f func(types.HostSet) types.LoadBalancer) {
	if lbFactories == nil {
		lbFactories = make(map[types.LoadBalancerType]func(types.HostSet) types.LoadBalancer)
	}
	lbFactories[lbType] = f
}

var rrFactory *roundRobinLoadBalancerFactory

func init() {
	rrFactory = &roundRobinLoadBalancerFactory{
		rand: rand.New(rand.NewSource(time.Now().UnixNano())),
	}
	RegisterLBType(types.RoundRobin, rrFactory.newRoundRobinLoadBalancer)
	RegisterLBType(types.Random, newRandomLoadBalancer)
	RegisterLBType(types.LeastActiveRequest, newleastActiveRequestLoadBalancer)
}

func NewLoadBalancer(lbType types.LoadBalancerType, hosts types.HostSet) types.LoadBalancer {
	if f, ok := lbFactories[lbType]; ok {
		return f(hosts)
	}
	return rrFactory.newRoundRobinLoadBalancer(hosts)
}

// LoadBalancer Implementations

type randomLoadBalancer struct {
	mutex sync.Mutex
	rand  *rand.Rand
	hosts types.HostSet
}

func newRandomLoadBalancer(hosts types.HostSet) types.LoadBalancer {
	return &randomLoadBalancer{
		rand:  rand.New(rand.NewSource(time.Now().UnixNano())),
		hosts: hosts,
	}
}

func (lb *randomLoadBalancer) ChooseHost(context types.LoadBalancerContext) types.Host {
	targets := lb.hosts.Hosts()
	total := len(targets)
	if total == 0 {
		return nil
	}
	lb.mutex.Lock()
	defer lb.mutex.Unlock()
	idx := lb.rand.Intn(total)
	for i := 0; i < total; i++ {
		host := targets[idx]
		if host.Health() {
			return host
		}
		idx = (idx + 1) % total
	}
	return nil
}

func (lb *randomLoadBalancer) IsExistsHosts(metadata api.MetadataMatchCriteria) bool {
	return len(lb.hosts.Hosts()) > 0
}

func (lb *randomLoadBalancer) HostNum(metadata api.MetadataMatchCriteria) int {
	return len(lb.hosts.Hosts())
}

type roundRobinLoadBalancer struct {
	hosts   types.HostSet
	rrIndex uint32
}

type roundRobinLoadBalancerFactory struct {
	mutex sync.Mutex
	rand  *rand.Rand
}

func (f *roundRobinLoadBalancerFactory) newRoundRobinLoadBalancer(hosts types.HostSet) types.LoadBalancer {
	var idx uint32
	hostsList := hosts.Hosts()
	f.mutex.Lock()
	defer f.mutex.Unlock()
	if len(hostsList) != 0 {
		idx = f.rand.Uint32() % uint32(len(hostsList))
	}
	return &roundRobinLoadBalancer{
		hosts:   hosts,
		rrIndex: idx,
	}
}

func (lb *roundRobinLoadBalancer) ChooseHost(context types.LoadBalancerContext) types.Host {
	targets := lb.hosts.Hosts()
	total := len(targets)
	if total == 0 {
		return nil
	}
	for i := 0; i < total; i++ {
		index := atomic.AddUint32(&lb.rrIndex, 1) % uint32(total)
		host := targets[index]
		if host.Health() {
			return host
		}
	}
	return nil
}

func (lb *roundRobinLoadBalancer) IsExistsHosts(metadata api.MetadataMatchCriteria) bool {
	return len(lb.hosts.Hosts()) > 0
}

func (lb *roundRobinLoadBalancer) HostNum(metadata api.MetadataMatchCriteria) int {
	return len(lb.hosts.Hosts())
}

// leastActiveRequestLoadBalancer choose the host with the least active request
type leastActiveRequestLoadBalancer struct {
	*EdfLoadBalancer
	choice int
}

func newleastActiveRequestLoadBalancer(hosts types.HostSet) types.LoadBalancer {
	lb := &leastActiveRequestLoadBalancer{}
	// TODO init the choice num with the least request config
	lb.choice = 2
	lb.EdfLoadBalancer = newEdfLoadBalancerLoadBalancer(hosts, lb.unweightChooseHost)
	return lb
}

func (lb *leastActiveRequestLoadBalancer) unweightChooseHost(context types.LoadBalancerContext) types.Host {

	healthyHosts := lb.hosts.HealthyHosts()
	healthyHostLen := len(healthyHosts)
	if healthyHostLen == 0 {
		// Return nil directly if healthyHosts is nil or size is 0
		return nil
	} else if healthyHostLen == 1 {
		// Return directly if there is only one host
		return healthyHosts[0]
	} else {
		lb.mutex.Lock()
		defer lb.mutex.Unlock()
		var candicate types.Host
		// Choose `choice` times and return the best one
		// See The Power of Two Random Choices: A Survey of Techniques and Results
		//  http://www.eecs.harvard.edu/~michaelm/postscripts/handbook2001.pdf
		for cur := 0; cur < lb.choice; cur++ {

			randIdx := lb.rand.Intn(healthyHostLen)
			tempHost := healthyHosts[randIdx]
			if candicate == nil {
				candicate = tempHost
				continue
			}
			if candicate.HostStats().UpstreamRequestActive.Count() > tempHost.HostStats().UpstreamRequestActive.Count() {
				candicate = tempHost
			}
		}
		return candicate
	}

}

// TODO:
// WRR

type EdfLoadBalancer struct {
	scheduler *edfSchduler
	hosts     types.HostSet
	rand      *rand.Rand
	mutex     sync.Mutex
	// the method to choose host when all host
	unweightChoose func(types.LoadBalancerContext) types.Host
	HostWight      func(host types.Host) uint32
}

func (lb *EdfLoadBalancer) ChooseHost(context types.LoadBalancerContext) types.Host {

	if lb.scheduler != nil {
		// do weight selection
		host := lb.scheduler.Next().(types.Host)
		return host
	} else {
		// do unweight selection
		return lb.unweightChoose(context)
	}
}

func (lb *EdfLoadBalancer) IsExistsHosts(metadata api.MetadataMatchCriteria) bool {
	return len(lb.hosts.Hosts()) > 0
}

func (lb *EdfLoadBalancer) HostNum(metadata api.MetadataMatchCriteria) int {
	return len(lb.hosts.Hosts())
}

func newEdfLoadBalancerLoadBalancer(hosts types.HostSet, unWeightChoose func(types.LoadBalancerContext) types.Host) *EdfLoadBalancer {
	lb := &EdfLoadBalancer{
		hosts:          hosts,
		rand:           rand.New(rand.NewSource(time.Now().UnixNano())),
		unweightChoose: unWeightChoose,
	}
	lb.refresh(hosts.HealthyHosts())
	return lb
}

func (lb *EdfLoadBalancer) refresh(hosts []types.Host) {
	// Check if the original host weights are equal and skip EDF creation if they are
	if hostWeightsAreEqual(hosts) {
		return
	}

	lb.scheduler = newEdfScheduler()

	// Init Edf scheduler with healthy hosts.
	for _, host := range hosts {
		lb.scheduler.Add(host, lb.HostWight(host))
	}

}

func hostWeightsAreEqual(hosts []types.Host) bool {
	if len(hosts) <= 1 {
		return true
	}
	weight := hosts[0].Weight()

	for i := 1; i < len(hosts); i++ {
		if hosts[i].Weight() != weight {
			return false
		}
	}
	return true
}
