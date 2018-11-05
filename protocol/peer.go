package protocol

import (
	"crypto/aes"
	"crypto/cipher"
	"encoding/binary"
	"github.com/monnand/dhkx"
	"github.com/pkg/errors"
)

type PendingPeer struct {
	Done        chan struct{}
	Established *EstablishedPeer
}

type EstablishedPeer struct {
	adapter     MessageAdapter
	kxState     KeyExchangeState
	dhGroup     *dhkx.DHGroup
	dhKeypair   *dhkx.DHKey
	aead        cipher.AEAD
	localNonce  uint64
	remoteNonce uint64
}

type KeyExchangeState byte

const (
	KeyExchange_Invalid KeyExchangeState = iota
	KeyExchange_PassivelyWaitForPublicKey
	KeyExchange_ActivelyWaitForPublicKey
	KeyExchange_Failed
	KeyExchange_Done
)

func prependSimpleSignature(idAdapter IdentityAdapter, data []byte) []byte {
	ret := make([]byte, idAdapter.SignatureSize()+len(data))
	copy(ret, idAdapter.Sign(data))
	copy(ret[idAdapter.SignatureSize():], data)
	return ret

}

func EstablishPeerWithMessageAdapter(c *Controller, idAdapter IdentityAdapter, adapter MessageAdapter, passive bool) (*EstablishedPeer, error) {
	g, err := dhkx.GetGroup(0)
	if err != nil {
		return nil, err
	}

	privKey, err := g.GeneratePrivateKey(nil)
	if err != nil {
		return nil, err
	}

	peer := &EstablishedPeer{
		adapter:   adapter,
		dhGroup:   g,
		dhKeypair: privKey,
	}
	if passive {
		peer.kxState = KeyExchange_PassivelyWaitForPublicKey
	} else {
		peer.kxState = KeyExchange_ActivelyWaitForPublicKey
		err = peer.adapter.SendMessage(c, prependSimpleSignature(idAdapter, peer.dhKeypair.Bytes()))
		if err != nil {
			return nil, err
		}
	}

	return peer, nil
}

func (p *EstablishedPeer) continueKeyExchange(c *Controller, idAdapter IdentityAdapter, raw []byte) error {
	switch p.kxState {
	case KeyExchange_ActivelyWaitForPublicKey, KeyExchange_PassivelyWaitForPublicKey:
		sig := raw[:idAdapter.SignatureSize()]
		rawPubKey := raw[idAdapter.SignatureSize():]
		if idAdapter.Verify(p.adapter.RemoteEndpoint(), rawPubKey, sig) == false {
			return errors.New("signature verification failed")
		}

		peerPubKey := dhkx.NewPublicKey(rawPubKey)
		sharedKey, err := p.dhGroup.ComputeKey(peerPubKey, p.dhKeypair)
		if err != nil {
			p.kxState = KeyExchange_Failed
			return err
		}

		if p.kxState == KeyExchange_PassivelyWaitForPublicKey {
			p.adapter.SendMessage(c, prependSimpleSignature(idAdapter, p.dhKeypair.Bytes())) // only sends the public key
		}

		p.dhGroup = nil
		p.dhKeypair = nil
		aesCipher, err := aes.NewCipher(sharedKey.Bytes())
		if err != nil {
			p.kxState = KeyExchange_Failed
			return err
		}
		aead, err := cipher.NewGCM(aesCipher) // FIXME
		if err != nil {
			p.kxState = KeyExchange_Failed
			return err
		}
		p.aead = aead
		p.kxState = KeyExchange_Done
		return nil
	case KeyExchange_Failed:
		return errors.New("failed previously")
	default:
		panic("unexpected key exchange state")
	}
}

func (p *EstablishedPeer) SendMessage(c *Controller, body []byte) error {
	// TODO: thread safety
	nonceBuffer := make([]byte, 12)
	binary.LittleEndian.PutUint64(nonceBuffer, p.localNonce)
	p.localNonce++

	cipherText := p.aead.Seal(nil, nonceBuffer, body, nil)
	return p.adapter.SendMessage(c, cipherText)
}

func (p *EstablishedPeer) UnwrapMessage(c *Controller, raw []byte) ([]byte, error) {
	nonceBuffer := make([]byte, 12)
	binary.LittleEndian.PutUint64(nonceBuffer, p.remoteNonce)
	p.remoteNonce++

	return p.aead.Open(nil, nonceBuffer, raw, nil)
}

func (p *EstablishedPeer) RemoteEndpoint() []byte {
	return p.adapter.RemoteEndpoint()
}
