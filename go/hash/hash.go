package hash

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"runtime"
	"sync"

	"github.com/rs/zerolog/log"

	storagetypes "github.com/evmos/evmos/v12/x/storage/types"
	"github.com/zkMeLabs/mechain-common/go/redundancy"
)

const (
	maxThreadNum   = 5
	jobChannelSize = 100
)

// IntegrityHasher compute integrityHash
type IntegrityHasher struct {
	ecDataHashes [][][]byte
	segHashes    [][]byte
	buffer       []byte
	segmentSize  int64
	dataShards   int
	parityShards int
	contentLen   int64
}

func NewHasher(size int64, data, parity int) *IntegrityHasher {
	return &IntegrityHasher{
		buffer:       make([]byte, 0),
		segmentSize:  size,
		dataShards:   data,
		parityShards: parity,
	}
}

// Init the integrityHash fields
func (i *IntegrityHasher) Init() {
	ecShards := i.dataShards + i.parityShards
	encodeDataHash := make([][][]byte, ecShards)
	for i := 0; i < ecShards; i++ {
		encodeDataHash[i] = make([][]byte, 0)
	}
	segChecksumList := make([][]byte, 0)
	i.ecDataHashes = encodeDataHash
	i.segHashes = segChecksumList
	if len(i.buffer) > 0 {
		i.buffer = i.buffer[:0]
	}
	i.contentLen = 0
}

// Append the data chunks to IntegrityHasher , the data size should be less than segment size
func (i *IntegrityHasher) Append(data []byte) error {
	dataSize := len(data)
	if dataSize > int(i.segmentSize) {
		return errors.New("the length of data size should be less than segmentSize")
	}
	if len(i.buffer) >= int(i.segmentSize) {
		return errors.New("the buffer of handler should be less than segmentSize")
	}
	originBuffer := make([]byte, len(i.buffer))
	copy(originBuffer, i.buffer)
	// use tempBuffer to store exceed data
	var tempBuffer []byte
	totalSize := int64(dataSize + len(i.buffer))
	if totalSize > i.segmentSize {
		index := dataSize - int(totalSize-i.segmentSize)
		tempBuffer = make([]byte, dataSize-index)
		copy(tempBuffer, data[index:])
		// buffer should be equal with segment size
		i.buffer = append(i.buffer, data[:index]...)
	} else {
		i.buffer = append(i.buffer, data...)
		// if buffer size less than segment size, just store the data
		if totalSize < i.segmentSize {
			return nil
		}
	}

	// compute segment hash
	if err := i.computeBufferHash(); err != nil {
		return err
	}

	// copy exceed data to buffer if exist
	if len(tempBuffer) > 0 {
		i.buffer = i.buffer[:0]
		i.buffer = append(i.buffer, tempBuffer...)
		return nil
	}

	i.buffer = i.buffer[:0]
	return nil
}

// Finish return the result of the Integrity hashes
func (i *IntegrityHasher) Finish() ([][]byte, int64, storagetypes.RedundancyType, error) {
	// deal with  remain content tot be computed
	if len(i.buffer) > 0 {
		if err := i.computeBufferHash(); err != nil {
			return nil, 0, storagetypes.REDUNDANCY_EC_TYPE, err
		}
	}

	hashList := make([][]byte, i.parityShards+i.dataShards+1)

	hashList[0] = GenerateIntegrityHash(i.segHashes)

	wg := &sync.WaitGroup{}
	spLen := len(i.ecDataHashes)
	wg.Add(spLen)
	for spID, content := range i.ecDataHashes {
		go func(data [][]byte, id int) {
			defer wg.Done()
			hashList[id+1] = GenerateIntegrityHash(data)
		}(content, spID)
	}
	wg.Wait()

	return hashList, i.contentLen, storagetypes.REDUNDANCY_EC_TYPE, nil
}

// computeBufferHash erasure encode the buffer of IntegrityHasher and compute the hash
func (i *IntegrityHasher) computeBufferHash() error {
	i.contentLen += int64(len(i.buffer))
	originBuffer := make([]byte, len(i.buffer))
	copy(originBuffer, i.buffer)
	// compute segment hash
	checksum := GenerateChecksum(i.buffer)
	i.segHashes = append(i.segHashes, checksum)
	// get erasure encoded bytes and compute pieces hashes
	encodeShards, err := redundancy.EncodeRawSegment(i.buffer, i.dataShards, i.parityShards)
	if err != nil {
		// recover buffer content if encode error
		i.buffer = i.buffer[:0]
		i.buffer = append(i.buffer, originBuffer...)
		return err
	}

	for index, shard := range encodeShards {
		// compute hash of pieces
		piecesHash := GenerateChecksum(shard)
		i.ecDataHashes[index] = append(i.ecDataHashes[index], piecesHash)
	}

	return nil
}

// ComputeIntegrityHash  return the integrity hash of file and data size
// If isSerial is true, compute the integrity hash using the serial version
// If isSerial is false or not provided, compute the integrity hash using the parallel version
func ComputeIntegrityHash(reader io.Reader, segmentSize int64, dataShards, parityShards int, isSerial bool) ([][]byte,
	int64, storagetypes.RedundancyType, error,
) {
	if isSerial {
		return ComputeIntegrityHashSerial(reader, segmentSize, dataShards, parityShards)
	}
	return ComputeIntegrityHashParallel(reader, segmentSize, dataShards, parityShards)
}

// ComputeIntegrityHashSerial split the reader into segment, ec encode the data, compute the hash roots of pieces in a serial way
// return the hash result array list and data size
func ComputeIntegrityHashSerial(reader io.Reader, segmentSize int64, dataShards, parityShards int) ([][]byte, int64,
	storagetypes.RedundancyType, error,
) {
	var segChecksumList [][]byte
	ecShards := dataShards + parityShards

	encodeDataHash := make([][][]byte, ecShards)
	for i := 0; i < ecShards; i++ {
		encodeDataHash[i] = make([][]byte, 0)
	}

	hashList := make([][]byte, ecShards+1)
	contentLen := int64(0)
	// read the data by segment segmentSize
	for {
		seg := make([]byte, segmentSize)
		n, err := reader.Read(seg)
		if err != nil {
			if err != io.EOF {
				log.Error().Msg("failed to read content:" + err.Error())
				return nil, 0, storagetypes.REDUNDANCY_EC_TYPE, err
			}
			break
		}

		if n > 0 && n <= int(segmentSize) {
			contentLen += int64(n)
			data := seg[:n]
			// compute segment hash
			checksum := GenerateChecksum(data)
			segChecksumList = append(segChecksumList, checksum)

			if err = encodeAndComputeHash(encodeDataHash, data, dataShards, parityShards); err != nil {
				return nil, 0, storagetypes.REDUNDANCY_EC_TYPE, err
			}
		}
	}

	// combine the hash root of pieces of the PrimarySP
	hashList[0] = GenerateIntegrityHash(segChecksumList)

	// compute the integrity hash of the SecondarySP
	wg := &sync.WaitGroup{}
	spLen := len(encodeDataHash)
	wg.Add(spLen)
	for spID, content := range encodeDataHash {
		go func(data [][]byte, id int) {
			defer wg.Done()
			hashList[id+1] = GenerateIntegrityHash(data)
		}(content, spID)
	}

	wg.Wait()

	return hashList, contentLen, storagetypes.REDUNDANCY_EC_TYPE, nil
}

func encodeAndComputeHash(encodeDataHash [][][]byte, segment []byte, dataShards, parityShards int) error {
	// get erasure encode bytes
	encodeShards, err := redundancy.EncodeRawSegment(segment, dataShards, parityShards)
	if err != nil {
		return err
	}

	for index, shard := range encodeShards {
		// compute hash of pieces
		piecesHash := GenerateChecksum(shard)
		encodeDataHash[index] = append(encodeDataHash[index], piecesHash)
	}

	return nil
}

// ComputerHashFromFile open a local file and compute hash result and segmentSize
func ComputerHashFromFile(filePath string, segmentSize int64, dataShards, parityShards int) ([][]byte, int64,
	storagetypes.RedundancyType, error,
) {
	f, err := os.Open(filePath)
	if err != nil {
		log.Error().Msg("failed to open file:" + err.Error())
		return nil, 0, storagetypes.REDUNDANCY_EC_TYPE, err
	}
	defer f.Close()

	return ComputeIntegrityHash(f, segmentSize, dataShards, parityShards, false)
}

// ComputerHashFromBuffer support computing hash and segmentSize from byte buffer
func ComputerHashFromBuffer(content []byte, segmentSize int64, dataShards, parityShards int) ([][]byte, int64,
	storagetypes.RedundancyType, error,
) {
	reader := bytes.NewReader(content)
	return ComputeIntegrityHash(reader, segmentSize, dataShards, parityShards, false)
}

// computePieceHashes encode the segment and return the hashes of ec pieces
func computePieceHashes(segment []byte, dataShards, parityShards int) ([][]byte, error) {
	// get erasure encode bytes
	encodeShards, err := redundancy.EncodeRawSegment(segment, dataShards, parityShards)
	if err != nil {
		return nil, err
	}

	var pieceChecksumList [][]byte
	for _, shard := range encodeShards {
		// compute hash of pieces
		piecesHash := GenerateChecksum(shard)
		pieceChecksumList = append(pieceChecksumList, piecesHash)
	}

	return pieceChecksumList, nil
}

// hashWorker receive the segment info and compute the corresponding segment hash and piece hashes.
// The result will be stored in the sync map to compute integrity hash in order.
func hashWorker(jobs <-chan SegmentInfo, errChan chan<- error, dataShards, parityShards int, wg *sync.WaitGroup,
	segmentHashMap *sync.Map, pieceHashMap *sync.Map,
) {
	defer wg.Done()

	for segInfo := range jobs {
		checksum := GenerateChecksum(segInfo.Data)
		segmentHashMap.Store(segInfo.SegmentID, checksum)

		pieceChecksumList, err := computePieceHashes(segInfo.Data, dataShards, parityShards)
		if err != nil {
			errChan <- err
			return
		}
		pieceHashMap.Store(segInfo.SegmentID, pieceChecksumList)
	}
}

// ComputeIntegrityHashParallel split the reader into segment, ec encode the data, compute the hash roots of pieces using
// return the hash result array list and data segmentSize
func ComputeIntegrityHashParallel(reader io.Reader, segmentSize int64, dataShards, parityShards int) ([][]byte, int64,
	storagetypes.RedundancyType, error,
) {
	var (
		segChecksumList [][]byte
		ecShards        = dataShards + parityShards
		contentLen      = int64(0)
		wg              sync.WaitGroup
	)
	// use sync.map to store the corresponding data of intermediate hash results and segment IDs
	segHashMap := &sync.Map{}
	pieceHashMap := &sync.Map{}
	encodeDataHash := make([][][]byte, ecShards)
	// store the result of integrity hash
	hashList := make([][]byte, ecShards+1)

	jobChan := make(chan SegmentInfo, jobChannelSize)
	errChan := make(chan error, 1)
	// the thread num should be less than maxThreadNum
	threadNum := runtime.NumCPU() / 2
	if threadNum > maxThreadNum {
		threadNum = maxThreadNum
	}
	// start workers to compute hash of each segment
	for i := 0; i < threadNum; i++ {
		wg.Add(1)
		go hashWorker(jobChan, errChan, dataShards, parityShards, &wg, segHashMap, pieceHashMap)
	}

	jobNum := 0
	for {
		seg := make([]byte, segmentSize)
		n, err := reader.Read(seg)
		if err != nil {
			if err != io.EOF {
				log.Error().Msg("failed to read content:" + err.Error())
				return nil, 0, storagetypes.REDUNDANCY_EC_TYPE, err
			}
			break
		}

		if n > 0 && n <= int(segmentSize) {
			contentLen += int64(n)
			data := seg[:n]
			// compute segment hash

			jobChan <- SegmentInfo{SegmentID: jobNum, Data: data}
			jobNum++
		}
	}
	close(jobChan)

	for i := 0; i < ecShards; i++ {
		encodeDataHash[i] = make([][]byte, jobNum)
	}

	wg.Wait()
	close(errChan)

	// check error
	for err := range errChan {
		if err != nil {
			log.Error().Msg("err chan detected err:" + err.Error())
			return nil, 0, storagetypes.REDUNDANCY_EC_TYPE, err
		}
	}

	for i := 0; i < jobNum; i++ {
		segHashValue, ok := segHashMap.Load(i)
		if !ok {
			return nil, 0, storagetypes.REDUNDANCY_EC_TYPE, fmt.Errorf("fail to load the segment hash")
		}
		segChecksumList = append(segChecksumList, segHashValue.([]byte))

		pieceHashValue, ok := pieceHashMap.Load(i)
		if !ok {
			return nil, 0, storagetypes.REDUNDANCY_EC_TYPE, fmt.Errorf("fail to load the segment hash")
		}
		hashValues := pieceHashValue.([][]byte)
		for j := 0; j < len(encodeDataHash); j++ {
			encodeDataHash[j][i] = hashValues[j]
		}
	}

	//  compute the integrity root of pieces of the PrimarySP
	hashList[0] = GenerateIntegrityHash(segChecksumList)

	// compute the integrity hash of the SecondarySPs
	spLen := len(encodeDataHash)
	wg.Add(spLen)
	for spID, content := range encodeDataHash {
		go func(data [][]byte, id int) {
			defer wg.Done()
			hashList[id+1] = GenerateIntegrityHash(data)
		}(content, spID)
	}

	wg.Wait()
	return hashList, contentLen, storagetypes.REDUNDANCY_EC_TYPE, nil
}
