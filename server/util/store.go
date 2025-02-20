package util

import (
	"context"
	"errors"

	blocks "github.com/ipfs/go-block-format"
	"github.com/ipfs/go-cid"
	"github.com/multiformats/go-multicodec"
	bitswapserver "github.com/willscott/go-selfish-bitswap-client/server"
)

var ErrNotHave = errors.New("not found")

func NewMemStore(of map[cid.Cid][]byte) bitswapserver.Blockstore {
	return &store{of}
}

type store struct {
	db map[cid.Cid][]byte
}

func (s *store) Has(ctx context.Context, c cid.Cid) (bool, error) {
	_, ok := s.db[c]
	if ok {
		return true, nil
	}
	return false, nil
}

func (s *store) Get(ctx context.Context, c cid.Cid) (blocks.Block, error) {
	blk, ok := s.db[c]
	if ok {
		return blocks.NewBlockWithCid(blk, c)
	}
	return nil, ErrNotHave
}

// TODO: To encode the blcoks here, take as input an encoder callback function to run on each array item.
func (s *store) GetAll() map[cid.Cid][]byte {
	return s.db
}

func Add(s bitswapserver.Blockstore, blk []byte) cid.Cid {
	st, ok := s.(*store)
	if !ok {
		return cid.Undef
	}

	name, err := cid.V1Builder{Codec: uint64(multicodec.Raw), MhType: uint64(multicodec.Sha2_256)}.Sum(blk)
	if err != nil {
		return cid.Undef
	}
	st.db[name] = blk
	return name
}
