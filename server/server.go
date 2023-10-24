package bitswapserver

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"time"

	blocks "github.com/ipfs/go-block-format"
	"github.com/ipfs/go-cid"
	"github.com/ipfs/go-log/v2"
	"github.com/libp2p/go-libp2p/core/host"
	"github.com/libp2p/go-libp2p/core/network"
	bitswap "github.com/willscott/go-selfish-bitswap-client"
	bitswap_message_pb "github.com/willscott/go-selfish-bitswap-client/message"
)

// accept bitswap streams. return requested blocks. simple

const (
	MaxRequestTimeout = 30 * time.Second
	MaxSendMsgSize    = 3 * 1024 * 1024
)

var (
	ErrNotHave  = errors.New("no requested blocks available")
	ErrOverflow = errors.New("send queue overflow")
)

var logger = log.Logger("bitswap-server")

type Blockstore interface {
	Has(ctx context.Context, c cid.Cid) (bool, error)
	Get(ctx context.Context, c cid.Cid) (blocks.Block, error)
}

func AttachBitswapServer(h host.Host, bs Blockstore) error {
	bsh := handler{bs}
	h.SetStreamHandler(bitswap.ProtocolBitswap, bsh.onStream)
	return nil
}

type handler struct {
	bs Blockstore
}

func (h *handler) onStream(s network.Stream) {
	if err := s.SetReadDeadline(time.Now().Add(MaxRequestTimeout)); err != nil {
		_ = s.Close()
		return
	}
	go h.readLoop(s)
}

func (h *handler) readLoop(stream network.Stream) {
	responder := &streamSender{stream, make(chan []byte, 5)}
	go responder.writeLoop()
	buf := make([]byte, 4*1024*1024)
	pos := uint64(0)
	prefixLen := 0
	msgLen := uint64(0)
	for {
		readLen, err := stream.Read(buf[pos:])

		if err != nil {
			if os.IsTimeout(err) {
				continue
			}
			if errors.Is(err, io.EOF) {
				return
			}
			//otherwise assume real error / conn closed.
			//s.connErr = err
			stream.Close()
			return
		}
		if msgLen == 0 {
			nextLen, intLen := binary.Uvarint(buf)
			if intLen <= 0 {
				//s.connErr = errors.New("invalid message")
				stream.Close()
				return
			}
			if nextLen > bitswap.MaxBlockSize {
				//s.connErr = errors.New("too large message")
				stream.Close()
				return
			}
			if nextLen > uint64(len(buf)) {
				nb := make([]byte, uint64(intLen)+nextLen)
				copy(nb, buf[:])
				buf = nb
			}
			msgLen = nextLen + uint64(intLen)
			pos = uint64(readLen)
			prefixLen = intLen
		} else {
			pos += uint64(readLen)
		}

		if pos == msgLen {
			if err := h.onMessage(responder, buf[prefixLen:msgLen]); err != nil {
				//s.connErr = fmt.Errorf("invalid block read: %w", err)
				stream.Close()
				return
			}
			pos = 0
			prefixLen = 0
			msgLen = 0
		}
	}
}

func (h *handler) processPIRRequestFromEncryptedCIDToIndex(encryptedCID []byte) (encryptedIndex []byte, err error) {
	encryptedIndex = make([]byte, 0)
	return encryptedIndex, nil
}

func (h *handler) processPIRRequestFromEncryptedIndexToBlock(encryptedIndex []byte) (encryptedBlock []byte, err error) {
	encryptedBlock = make([]byte, 0)
	return encryptedBlock, nil
}

func (h *handler) onMessage(ss *streamSender, buf []byte) error {
	m := bitswap_message_pb.Message{}
	if err := m.Unmarshal(buf); err != nil {
		logger.Warnw("failed to parse message as bitswap", "err", err)
		return fmt.Errorf("failed to parse message (len %d) as bitswap: %w", len(buf), err)
	}

	resp := bitswap_message_pb.Message{}
	resp.Wantlist = bitswap_message_pb.Message_Wantlist{}
	filled := 0
	timed, cncl := context.WithTimeout(context.Background(), time.Second)
	defer cncl()
	for _, e := range m.Wantlist.Entries {
		// Changes in function signatures: no block CIDs here
		// TODO: We'd need to process the encrypted CID and return an encrypted Index
		//  (instead of Message_Have) and then process the encrypted Block Request to return Block
		wantType := e.GetWantType().String()
		if wantType == "Block" {
			if filled < MaxSendMsgSize {
				data, err := h.bs.Get(timed, e.Block.Cid)
				if err != nil {
					return err
				}
				resp.Blocks = append(resp.Blocks, data.RawData())
				filled += len(data.RawData())
			} else { // either the wantType is "Have" or it is "Block" but we can't send the block in this message
				// in both cases just say that we have it
				resp.BlockPresences = append(resp.BlockPresences, bitswap_message_pb.Message_BlockPresence{
					Cid:  e.Block, // this just returns the CID from the request, not to be confused with the block fetched above
					Type: bitswap_message_pb.Message_Have,
				})
			}

		} else { // wantType == "Have"
			// just reply back whether we have the message or not
			if has, err := h.bs.Has(timed, e.Block.Cid); err == nil && has {
				resp.BlockPresences = append(resp.BlockPresences, bitswap_message_pb.Message_BlockPresence{
					Cid:  e.Block, // this just returns the CID from the request, not to be confused with the block fetched above
					Type: bitswap_message_pb.Message_Have,
				})
			} else if e.SendDontHave == true {
				resp.BlockPresences = append(resp.BlockPresences, bitswap_message_pb.Message_BlockPresence{
					Cid:  e.Block, // this just returns the CID from the request, not to be confused with the block fetched above
					Type: bitswap_message_pb.Message_DontHave,
				})
			}
		}
	}

	if filled > 0 {
		rBytes, err := resp.Marshal()
		if err != nil {
			return fmt.Errorf("marshal of response failed: %w", err)
		}
		return ss.enqueue(rBytes)
	} else {
		return ErrNotHave
	}
}

type streamSender struct {
	network.Stream
	queue chan []byte
}

func (ss *streamSender) enqueue(msg []byte) error {
	select {
	case ss.queue <- msg:
		return nil
	default:
		return ErrOverflow
	}
}

func (ss *streamSender) writeLoop() {
	next := []byte{}
	for {
		if len(next) > 0 {
			n, err := ss.Stream.Write(next)
			if err != nil {
				return
			}
			next = next[n:]
			continue
		}

		msg, ok := <-ss.queue
		buf := make([]byte, binary.MaxVarintLen64)
		ln := binary.PutUvarint(buf, uint64(len(msg)))
		if _, err := ss.Stream.Write(buf[0:ln]); err != nil {
			return
		}
		next = msg
		if !ok {
			return
		}
	}
}
