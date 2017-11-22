package retrieval

import (
	"encoding/binary"
	"errors"
	"io"
	"sync"

	"code.opsmind.com/common/task"
	"github.com/golang/protobuf/proto"
	dto "github.com/prometheus/client_model/go"
)

func PbToMetricFamilies(wp *task.WorkerPool, r io.Reader) ([]*dto.MetricFamily, error) {
	proj := wp.NewProject()
	ret := make([]*dto.MetricFamily, 0, 1024)
	wlock := sync.Mutex{}

	// dispatcher
	var err error
	ch := make(chan []byte, 1024)
	go func() {
		for {
			if err != nil {
				return
			}
			b, e := ReadDelimited(r)
			if e != nil {
				if e != io.EOF {
					err = e
				}
				close(ch)
				return
			}
			ch <- b
		}
	}()
	decode := func() {
		for {
			mf := &dto.MetricFamily{}
			select {
			case b, ok := <-ch:
				if !ok {
					return
				}
				if e := proto.Unmarshal(b, mf); e != nil {
					err = e
					return
				}
				wlock.Lock()
				ret = append(ret, mf)
				wlock.Unlock()
			}
		}
	}
	for i := 0; i < wp.Len(); i++ {
		proj.AddTask(decode)
	}
	proj.Wait()
	return ret, err
}

var errInvalidVarint = errors.New("invalid varint32 encountered")

func ReadDelimited(r io.Reader) (b []byte, err error) {
	var headerBuf [binary.MaxVarintLen32]byte
	var bytesRead, varIntBytes int
	var messageLength uint64
	for varIntBytes == 0 { // i.e. no varint has been decoded yet.
		if bytesRead >= len(headerBuf) {
			return nil, errInvalidVarint
		}
		newBytesRead, err := r.Read(headerBuf[bytesRead : bytesRead+1])
		if newBytesRead == 0 {
			if err != nil {
				return nil, err
			}
			continue
		}
		bytesRead += newBytesRead
		messageLength, varIntBytes = proto.DecodeVarint(headerBuf[:bytesRead])
	}

	b = make([]byte, messageLength)
	_, err = io.ReadFull(r, b)
	if err != nil {
		return nil, err
	}
	return b, nil
}
