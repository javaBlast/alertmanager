// Copyright 2013 Prometheus Team
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
// http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package manager

import (
	"encoding/json"
	"fmt"
	"io/ioutil"
	"log"
	"sync"
	"time"
)

type SilenceId uint
type Silences []*Silence

type Silence struct {
	// The numeric ID of the silence.
	Id SilenceId
	// Name/email of the silence creator.
	CreatedBy string
	// When the silence was first created (Unix timestamp).
	CreatedAt time.Time
	// When the silence expires (Unix timestamp).
	EndsAt time.Time
	// Additional comment about the silence.
	Comment string
	// Filters that determine which events are silenced.
	Filters Filters
	// Timer used to trigger the deletion of the Silence after its expiry
	// time.
	expiryTimer *time.Timer
}

type ApiSilence struct {
	Id               SilenceId
	CreatedBy        string
	CreatedAtSeconds int64
	EndsAtSeconds    int64
	Comment          string
	Filters          map[string]string
}

func (s *Silence) MarshalJSON() ([]byte, error) {
	filters := map[string]string{}
	for _, f := range s.Filters {
		name := f.Name.String()[1 : len(f.Name.String())-1]
		value := f.Value.String()[1 : len(f.Value.String())-1]
		filters[name] = value
	}

	return json.Marshal(&ApiSilence{
		Id:               s.Id,
		CreatedBy:        s.CreatedBy,
		CreatedAtSeconds: s.CreatedAt.Unix(),
		EndsAtSeconds:    s.EndsAt.Unix(),
		Comment:          s.Comment,
		Filters:          filters,
	})
}

func (s *Silence) UnmarshalJSON(data []byte) error {
	sc := &ApiSilence{}
	json.Unmarshal(data, sc)

	filters := make(Filters, 0, len(sc.Filters))
	for label, value := range sc.Filters {
		filters = append(filters, NewFilter(label, value))
	}

	if sc.CreatedAtSeconds == 0 {
		sc.CreatedAtSeconds = time.Now().Unix()
	}
	if sc.EndsAtSeconds == 0 {
		sc.EndsAtSeconds = time.Now().Add(time.Hour).Unix()
	}

	*s = Silence{
		Id:        sc.Id,
		CreatedBy: sc.CreatedBy,
		CreatedAt: time.Unix(sc.CreatedAtSeconds, 0).UTC(),
		EndsAt:    time.Unix(sc.EndsAtSeconds, 0).UTC(),
		Comment:   sc.Comment,
		Filters:   filters,
	}
	return nil
}

type Silencer struct {
	// Silences managed by this Silencer.
	Silences map[SilenceId]*Silence
	// Used to track the next Silence Id to allocate.
	lastId SilenceId

	// Mutex to protect the above.
	mu sync.Mutex
}

type IsInhibitedInterrogator interface {
	IsInhibited(*Event) (bool, *Silence)
}

func NewSilencer() *Silencer {
	return &Silencer{
		Silences: make(map[SilenceId]*Silence),
	}
}

func (s *Silencer) nextSilenceId() SilenceId {
	s.lastId++
	return s.lastId
}

func (s *Silencer) setupExpiryTimer(sc *Silence) {
	if sc.expiryTimer != nil {
		sc.expiryTimer.Stop()
	}
	expDuration := sc.EndsAt.Sub(time.Now())
	sc.expiryTimer = time.AfterFunc(expDuration, func() {
		if err := s.DelSilence(sc.Id); err != nil {
			log.Printf("Failed to delete silence %d: %s", sc.Id, err)
		}
	})
}

func (s *Silencer) AddSilence(sc *Silence) SilenceId {
	s.mu.Lock()
	defer s.mu.Unlock()

	if sc.Id == 0 {
		sc.Id = s.nextSilenceId()
	} else {
		if sc.Id > s.lastId {
			s.lastId = sc.Id
		}
	}

	s.setupExpiryTimer(sc)
	s.Silences[sc.Id] = sc
	return sc.Id
}

func (s *Silencer) UpdateSilence(sc *Silence) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	origSilence, ok := s.Silences[sc.Id]
	if !ok {
		return fmt.Errorf("Silence with ID %d doesn't exist", sc.Id)
	}
	if sc.EndsAt != origSilence.EndsAt {
		origSilence.expiryTimer.Stop()
	}
	*origSilence = *sc
	s.setupExpiryTimer(origSilence)
	return nil
}

func (s *Silencer) GetSilence(id SilenceId) (*Silence, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	sc, ok := s.Silences[id]
	if !ok {
		return nil, fmt.Errorf("Silence with ID %d doesn't exist", id)
	}
	return sc, nil
}

func (s *Silencer) DelSilence(id SilenceId) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if _, ok := s.Silences[id]; !ok {
		return fmt.Errorf("Silence with ID %d doesn't exist", id)
	}
	delete(s.Silences, id)
	return nil
}

func (s *Silencer) SilenceSummary() Silences {
	s.mu.Lock()
	defer s.mu.Unlock()

	silences := make(Silences, 0, len(s.Silences))
	for _, sc := range s.Silences {
		silences = append(silences, sc)
	}
	return silences
}

func (s *Silencer) IsInhibited(e *Event) (bool, *Silence) {
	s.mu.Lock()
	defer s.mu.Unlock()

	for _, s := range s.Silences {
		if s.Filters.Handles(e) {
			return true, s
		}
	}
	return false, nil
}

// Loads a JSON representation of silences from a file.
func (s *Silencer) LoadFromFile(fileName string) error {
	silenceJson, err := ioutil.ReadFile(fileName)
	if err != nil {
		return err
	}
	silences := Silences{}
	if err = json.Unmarshal(silenceJson, &silences); err != nil {
		return err
	}
	for _, sc := range silences {
		s.AddSilence(sc)
	}
	return nil
}

// Saves a JSON representation of silences to a file.
func (s *Silencer) SaveToFile(fileName string) error {
	silenceSummary := s.SilenceSummary()

	resultBytes, err := json.Marshal(silenceSummary)
	if err != nil {
		return err
	}
	return ioutil.WriteFile(fileName, resultBytes, 0644)
}

func (s *Silencer) Close() {
	s.mu.Lock()
	defer s.mu.Unlock()

	for _, sc := range s.Silences {
		if sc.expiryTimer != nil {
			sc.expiryTimer.Stop()
		}
	}
}