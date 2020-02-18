// Copyright 2013 The Prometheus Authors
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

package stats

import (
	"encoding/json"
	"sort"
)

// QueryTiming identifies the code area or functionality in which time is spent
// during a query.
type QueryTiming int

// Query timings.
const (
	TotalEvalTime QueryTiming = iota
	ResultSortTime
	QueryPreparationTime
	InnerEvalTime
	ResultAppendTime
	ExecQueueTime
)

// Return a string representation of a QueryTiming identifier.
func (s QueryTiming) String() string {
	switch s {
	case TotalEvalTime:
		return "TotalEval"
	case ResultSortTime:
		return "ResultSorting"
	case QueryPreparationTime:
		return "QueryPreparation"
	case InnerEvalTime:
		return "InnerEval"
	case ResultAppendTime:
		return "ResultAppend"
	case ExecQueueTime:
		return "ExecQueueWait"
	default:
		return "UnknownQueryTiming"
	}
}

type QueryStats struct {
	*TimerGroup

	SeriesScanned int64
	SeriesCovered int64
}

func NewQueryStats() *QueryStats {
	return &QueryStats{TimerGroup: NewTimerGroup()}
}

func (p *QueryStats) MarshalJSON() ([]byte, error) {
	timers := Timers{}
	for _, timer := range p.timers {
		timers = append(timers, timer)
	}
	sort.Sort(byCreationTimeSorter{timers})

	returnStats := struct {
		Timers        Timers `json:"timers"`
		SeriesScanned int64  `json:"series_scanned"`
		SeriesCovered int64  `json:"series_covered"`
	}{
		Timers:        timers,
		SeriesScanned: p.SeriesScanned,
		SeriesCovered: p.SeriesCovered,
	}
	return json.Marshal(returnStats)
}

func (p *QueryStats) UnmarshalJSON(d []byte) error {
	panic("not implemented(cannot unmarshal into a QueryStats)")
}
