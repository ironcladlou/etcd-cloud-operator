package status

import (
	"context"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/quentin-m/etcd-cloud-operator/pkg/providers/asg"
	"go.uber.org/zap"
)

type OperatorStatus struct {
	States   map[string]int
	IsSeeder bool
}

func (e *OperatorStatus) IsValid() bool {
	return len(e.States) > 0
}

type OperatorCache struct {
	value OperatorStatus
	mu    sync.Mutex

	client     *http.Client
	asgCache   *AsgCache
	serverPort int
}

func NewOperatorCache(client *http.Client, port int, asgCache *AsgCache) *OperatorCache {
	return &OperatorCache{
		value: OperatorStatus{
			States: make(map[string]int),
		},
		client:     client,
		asgCache:   asgCache,
		serverPort: port,
	}
}

func (c *OperatorCache) Run(ctx context.Context) {
	for {
		currAsg := c.asgCache.Value()
		if currAsg.IsValid() {
			start := time.Now()
			isSeeder, states := fetchEcoStatuses(c.client, c.serverPort, currAsg.Instances, currAsg.Self)
			zap.S().With("asg", currAsg).Debugf("operator status evaluation completed in %s", time.Since(start).Round(time.Millisecond))
			c.mu.Lock()
			c.value = OperatorStatus{
				States:   states,
				IsSeeder: isSeeder,
			}
			c.mu.Unlock()
		}
		select {
		case <-time.After(5 * time.Second):
		case <-ctx.Done():
			return
		}
	}
}

func (c *OperatorCache) Value() OperatorStatus {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.value
}

type status struct {
	instance asg.Instance

	State    string `json:"state"`
	Revision int64  `json:"revision"`
}

func fetchEcoStatuses(httpClient *http.Client, port int, asgInstances []asg.Instance, asgSelf asg.Instance) (bool, map[string]int) {
	var wg sync.WaitGroup
	var mu sync.Mutex
	wg.Add(len(asgInstances))

	// Fetch ECO statuses.
	var ecoStatuses []*status
	for _, asgInstance := range asgInstances {
		go func(asgInstance asg.Instance) {
			defer wg.Done()

			start := time.Now()
			st, err := fetchStatus(httpClient, port, asgInstance)
			if err != nil {
				zap.S().With(zap.Error(err)).Warnf("failed to query %s's ECO instance", asgInstance.Name())
				st = &status{
					instance: asgInstance,
					State:    "UNKNOWN",
					Revision: 0,
				}
			}

			mu.Lock()
			defer mu.Unlock()

			zap.S().Debugf("operator status check for instance %s completed in %s", asgInstance.Name(), time.Since(start).Round(time.Millisecond))
			ecoStatuses = append(ecoStatuses, st)
		}(asgInstance)
	}
	wg.Wait()

	// Sort the ECO statuses so we can systematically find the identity of the seeder.
	sort.Slice(ecoStatuses, func(i, j int) bool {
		if ecoStatuses[i].Revision == ecoStatuses[j].Revision {
			return ecoStatuses[i].instance.Name() < ecoStatuses[j].instance.Name()
		}
		return ecoStatuses[i].Revision < ecoStatuses[j].Revision
	})

	// Count ECO statuses and determine if we are the seeder.
	ecoStates := make(map[string]int)
	for _, ecoStatus := range ecoStatuses {
		if _, ok := ecoStates[ecoStatus.State]; !ok {
			ecoStates[ecoStatus.State] = 0
		}
		ecoStates[ecoStatus.State]++
	}

	var statuses []string
	for _, status := range ecoStatuses {
		statuses = append(statuses, fmt.Sprintf("name=%s, address=%s, bindAddress=%s, state=%s, rev=%d",
			status.instance.Name(), status.instance.Address(), status.instance.BindAddress(), status.State, status.Revision))
	}
	var instances []string
	for _, inst := range asgInstances {
		instances = append(instances, fmt.Sprintf("name=%s, address=%s, bindAddress=%s", inst.Name(), inst.Address(), inst.BindAddress()))
	}
	zap.S().Debugf("self=%s, states=%v, statuses=[%s], instances=[%s]", asgSelf.Name(), ecoStates, strings.Join(statuses, ";"), strings.Join(instances, ";"))

	return ecoStatuses[len(ecoStatuses)-1].instance.Name() == asgSelf.Name(), ecoStates
}

func fetchStatus(httpClient *http.Client, port int, instance asg.Instance) (*status, error) {
	var st = status{
		instance: instance,
		State:    "UNKNOWN",
		Revision: 0,
	}

	resp, err := httpClient.Get(fmt.Sprintf("http://%s:%d/status", instance.Address(), port))
	if err != nil {
		return &st, err
	}
	defer resp.Body.Close()

	b, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		return &st, err
	}

	err = json.Unmarshal(b, &st)
	return &st, err
}
