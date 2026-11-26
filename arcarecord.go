package main

// Reading an Arca coin record's own leaf. A coin's record is the binary form
// of arca's CoinRecord (covenant/src/transfer.rs, `write` and `read`): a
// version byte, then one record. Version 1 is the plain form; version 2 is
// written when an input in the record has its owner's exit, and adds a flag
// after each input's signatures. Both are read, with every record tag arca
// has written: a batch leaf (0), a board (2), an accumulator slot (6), a fill
// (4), and a transfer whose tag names its leaf's template (1 vtxo-1, 3
// htlc-1, 5 and 9 landing, 7 enclave-1, 8 vtxo-2, 10 htlc-2). A transfer's record writes the full records
// of the coins it spent before its own leaf, so the owner key and nonce of
// every coin in its history appear in it, ahead of its own. Searching the
// bytes for a code's owner key and nonce therefore finds the history as
// readily as the coin. ownLeaf instead reads the record field by field as
// the wallet reads it, skipping the history by its structure, and gives the
// owner key and nonce at the place of the coin's own leaf: what the payer's
// wallet wrote as the receiver, and what the receiving wallet checked before
// it accepted the coin. Nothing in the history, the outputs or the terms
// after the leaf is ever read as an owner, whatever bytes a payer puts
// there, and a record that does not parse to its last byte gives nothing.

import (
	"encoding/binary"
	"encoding/hex"
	"errors"
)

// The limits arca's reader enforces (transfer.rs, leaf.rs, offboard.rs,
// landing.rs).
const (
	coinRecordVersion     = 1
	coinRecordVersionExit = 2
	recMaxHops            = 16
	recMaxInputs          = 16
	recMaxOutputs         = 4
	recMaxFillOutputs     = 4
	recMaxDestination     = 10_000
	recProgramBytes       = 20
)

var errRecord = errors.New("not a coin record this reader knows")

type recReader struct {
	b   []byte
	pos int
	v2  bool // format version 2: an exit flag after each input
}

func (r *recReader) take(n int) ([]byte, error) {
	if n < 0 || r.pos+n > len(r.b) {
		return nil, errRecord
	}
	s := r.b[r.pos : r.pos+n]
	r.pos += n
	return s, nil
}

func (r *recReader) skip(n int) error { _, err := r.take(n); return err }

func (r *recReader) u8() (int, error) {
	s, err := r.take(1)
	if err != nil {
		return 0, err
	}
	return int(s[0]), nil
}

func (r *recReader) u16() (int, error) {
	s, err := r.take(2)
	if err != nil {
		return 0, err
	}
	return int(binary.LittleEndian.Uint16(s)), nil
}

// compactSize is Bitcoin's compact size, as arca writes it.
func (r *recReader) compactSize() (uint64, error) {
	n, err := r.u8()
	if err != nil {
		return 0, err
	}
	var s []byte
	switch n {
	case 0xfd:
		s, err = r.take(2)
		if err == nil {
			return uint64(binary.LittleEndian.Uint16(s)), nil
		}
	case 0xfe:
		s, err = r.take(4)
		if err == nil {
			return uint64(binary.LittleEndian.Uint32(s)), nil
		}
	case 0xff:
		s, err = r.take(8)
		if err == nil {
			return binary.LittleEndian.Uint64(s), nil
		}
	default:
		return uint64(n), nil
	}
	return 0, err
}

// recLeaf is a coin's own leaf: its owner key (empty for a leaf a fill
// credits back to its maker, which names a nonce only) and nonce, in hex.
type recLeaf struct {
	Owner, Nonce string
}

// ownLeaf reads the owner key and nonce of the coin a record is the record
// of. named is false for a record that names no leaf owner (a board, an
// accumulator slot); err is set for a record this reader cannot read to its
// last byte, which is never matched to anything.
func ownLeaf(raw []byte) (l recLeaf, named bool, err error) {
	r := &recReader{b: raw}
	v, err := r.u8()
	if err != nil || (v != coinRecordVersion && v != coinRecordVersionExit) {
		return recLeaf{}, false, errRecord
	}
	r.v2 = v == coinRecordVersionExit
	own, _, err := r.record(0)
	if err != nil || r.pos != len(r.b) {
		return recLeaf{}, false, errRecord
	}
	if own == nil {
		return recLeaf{}, false, nil
	}
	return *own, true, nil
}

// record reads one record and gives its own leaf, when it names one, and
// its tag.
func (r *recReader) record(depth int) (*recLeaf, int, error) {
	if depth > recMaxHops {
		return nil, 0, errRecord
	}
	tag, err := r.u8()
	if err != nil {
		return nil, 0, err
	}
	l, err := r.body(tag, depth)
	return l, tag, err
}

func (r *recReader) body(tag, depth int) (*recLeaf, error) {
	switch tag {
	case 0: // a round's leaf: u16 length, the leaf record, preimage, authorisations
		n, err := r.u16()
		if err != nil {
			return nil, err
		}
		leaf, err := r.take(n)
		if err != nil {
			return nil, err
		}
		if err := r.skip(32); err != nil {
			return nil, err
		}
		k, err := r.u8()
		if err != nil {
			return nil, err
		}
		if err := r.skip(k * (64 + 4)); err != nil {
			return nil, err
		}
		// LeafRecord: version, template id, template version, owner, owner nonce, ...
		if len(leaf) < 3+64 {
			return nil, errRecord
		}
		return &recLeaf{hex.EncodeToString(leaf[3:35]), hex.EncodeToString(leaf[35:67])}, nil
	case 6: // an accumulator slot
		n, err := r.u16()
		if err != nil {
			return nil, err
		}
		return nil, r.skip(n + 32 + 64 + 4)
	case 2: // a board
		n, err := r.u16()
		if err != nil {
			return nil, err
		}
		return nil, r.skip(n)
	case 1, 3, 5, 7, 8, 9, 10: // a transfer
		if err := r.inputs(depth); err != nil {
			return nil, err
		}
		m, err := r.outputs(recMaxOutputs)
		if err != nil {
			return nil, err
		}
		if idx, err := r.u8(); err != nil || idx >= m {
			return nil, errRecord
		}
		kn, err := r.take(64) // owner, owner nonce
		if err != nil {
			return nil, err
		}
		if err := r.skip(32 + 2); err != nil { // creator nonce, exit delay
			return nil, err
		}
		switch tag {
		case 3: // htlc-1: direction, payment hash, timeout, operator delay
			err = r.skip(1 + 32 + 4 + 2)
		case 5, 9: // landing: program, allowance
			err = r.skip(recProgramBytes + 8)
		case 10: // htlc-2: payment hash, timeout, refunder, refund delay
			err = r.skip(32 + 4 + 32 + 2)
		case 7: // enclave-1: holder, policy, a clawback flag and key
			if err = r.skip(64); err == nil {
				var f int
				if f, err = r.u8(); err == nil {
					switch f {
					case 0:
					case 1:
						err = r.skip(32)
					default:
						err = errRecord
					}
				}
			}
		}
		if err != nil {
			return nil, err
		}
		return &recLeaf{hex.EncodeToString(kn[:32]), hex.EncodeToString(kn[32:])}, nil
	case 4: // a fill of an order
		if _, _, err := r.record(depth + 1); err != nil {
			return nil, err
		}
		if err := r.skip(64 + 1); err != nil { // signature, partial
			return nil, err
		}
		if err := r.inputs(depth); err != nil {
			return nil, err
		}
		m, err := r.outputs(recMaxFillOutputs)
		if err != nil || m < 2 {
			return nil, errRecord
		}
		if idx, err := r.u8(); err != nil || idx >= m {
			return nil, errRecord
		}
		kind, err := r.u8()
		if err != nil {
			return nil, err
		}
		switch kind {
		case 0, 1: // credited back to the maker, or the order's rest: a nonce, the maker's key
			n, err := r.take(32)
			if err != nil {
				return nil, err
			}
			return &recLeaf{Nonce: hex.EncodeToString(n)}, nil
		case 2: // the taker's new leaf
			kn, err := r.take(64)
			if err != nil {
				return nil, err
			}
			if err := r.skip(32 + 2); err != nil {
				return nil, err
			}
			return &recLeaf{hex.EncodeToString(kn[:32]), hex.EncodeToString(kn[32:])}, nil
		}
		return nil, errRecord
	}
	return nil, errRecord
}

// inputs skips a transfer's inputs: each a full record, its checkpoint
// value and four signatures, in version 2 an exit flag, and for an htlc-2
// coin how it is spent (a claim with its preimage, or a refund).
func (r *recReader) inputs(depth int) error {
	n, err := r.u8()
	if err != nil {
		return err
	}
	if n == 0 || n > recMaxInputs {
		return errRecord
	}
	for i := 0; i < n; i++ {
		_, tag, err := r.record(depth + 1)
		if err != nil {
			return err
		}
		if err := r.skip(8 + 4*64); err != nil {
			return err
		}
		if r.v2 {
			if f, err := r.u8(); err != nil || f > 1 {
				return errRecord
			}
		}
		if tag == 10 {
			switch f, err := r.u8(); {
			case err != nil:
				return err
			case f == 0:
				if err := r.skip(32); err != nil {
					return err
				}
			case f != 1:
				return errRecord
			}
		}
	}
	return nil
}

// outputs skips a transfer's outputs and gives their count.
func (r *recReader) outputs(max int) (int, error) {
	m, err := r.u8()
	if err != nil {
		return 0, err
	}
	if m == 0 || m > max {
		return 0, errRecord
	}
	for i := 0; i < m; i++ {
		if err := r.skip(32 + 8); err != nil {
			return 0, err
		}
		n, err := r.compactSize()
		if err != nil || n > recMaxDestination {
			return 0, errRecord
		}
		if err := r.skip(int(n)); err != nil {
			return 0, err
		}
	}
	return m, nil
}
