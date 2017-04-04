// Copyright 2015 Keybase, Inc. All rights reserved. Use of
// this source code is governed by the included BSD license.

package saltpack

import (
	"bytes"
	"crypto/sha512"
	"io"

	"golang.org/x/crypto/nacl/secretbox"
)

type signcryptSealStream struct {
	output        io.Writer
	encoder       encoder
	header        *SigncryptionHeader
	encryptionKey SymmetricKey
	signingKey    SigningSecretKey
	keyring       Keyring
	buffer        bytes.Buffer
	inblock       []byte
	headerHash    []byte

	numBlocks encryptionBlockNumber // the lower 64 bits of the nonce

	didHeader bool
	eof       bool
	err       error
}

func (sss *signcryptSealStream) Write(plaintext []byte) (int, error) {

	if sss.err != nil {
		return 0, sss.err
	}

	var ret int
	if ret, sss.err = sss.buffer.Write(plaintext); sss.err != nil {
		return 0, sss.err
	}
	for sss.buffer.Len() >= encryptionBlockSize {
		sss.err = sss.signcryptBlock()
		if sss.err != nil {
			return 0, sss.err
		}
	}
	return ret, nil
}

func (sss *signcryptSealStream) signcryptBlock() error {
	var n int
	var err error
	n, err = sss.buffer.Read(sss.inblock[:])
	if err != nil {
		return err
	}
	return sss.signcryptBytes(sss.inblock[0:n])
}

func (sss *signcryptSealStream) signcryptBytes(b []byte) error {

	if err := sss.numBlocks.check(); err != nil {
		return err
	}

	nonce := nonceForChunkSigncryption(sss.numBlocks)

	plaintextHash := sha512.Sum512(b)

	signatureInput := []byte(signatureEncryptedString)
	signatureInput = append(signatureInput, sss.headerHash...)
	signatureInput = append(signatureInput, nonce[:]...)
	signatureInput = append(signatureInput, plaintextHash[:]...)

	detachedSig, err := sss.signingKey.Sign(signatureInput)
	if err != nil {
		return err
	}

	attachedSig := append(detachedSig, b...)

	ciphertext := secretbox.Seal([]byte{}, attachedSig, (*[24]byte)(nonce), (*[32]byte)(&sss.encryptionKey))

	block := []interface{}{ciphertext}

	if err := sss.encoder.Encode(block); err != nil {
		return err
	}

	sss.numBlocks++
	return nil
}

func receiverEntryForBoxKey(receiverBoxKey BoxPublicKey, ephemeralPriv BoxSecretKey, payloadKey SymmetricKey, index uint64) receiverKeys {
	// Derive a shared secret by encrypting zeroes, as in the encryption
	// format. Multiple recipients could potentially claim the same public key
	// and share this secret.
	sharedSecretBox := ephemeralPriv.Box(receiverBoxKey, nonceForDerivedSharedKey(), make([]byte, 32))
	derivedKey, err := rawBoxKeyFromSlice(sharedSecretBox[len(sharedSecretBox)-32 : len(sharedSecretBox)])
	if err != nil {
		panic(err) // should be statically impossible, if the slice above is the right length
	}

	// Compute the identifier that the receiver will use to find this entry.
	// Include the recipient index, so that this identifier is unique even if
	// two recipients claim the same public key (though unfortunately that
	// means that recipients will need to recompute the identifier for each
	// entry in the recipients list). This identifier is somewhat redundant,
	// because a recipient could instead just attempt to decrypt the payload
	// key secretbox and see if it works, but including them adds a bit to
	// anonymity by making box key recipients indistinguishable from symmetric
	// key recipients.
	keyIdentifierDigest := sha512.New()
	keyIdentifierDigest.Write([]byte("saltpack signcryption derived key identifier\x00"))
	keyIdentifierDigest.Write(derivedKey[:])
	keyIdentifierDigest.Write(nonceForPayloadKeyBoxV2(index)[:])
	identifier := keyIdentifierDigest.Sum(nil)[0:32]

	payloadKeyBox := secretbox.Seal(
		nil,
		payloadKey[:],
		(*[24]byte)(nonceForPayloadKeyBoxV2(index)),
		(*[32]byte)(derivedKey))

	return receiverKeys{
		ReceiverKID:   identifier,
		PayloadKeyBox: payloadKeyBox,
	}
}

type ReceiverSymmetricKey struct {
	// In practice these identifiers will be KBFS TLF keys.
	Key SymmetricKey
	// In practice these identifiers will be KBFS TLF pseudonyms.
	Identifier []byte
}

func receiverEntryForSymmetricKey(receiverSymmetricKey ReceiverSymmetricKey, ephemeralPub BoxPublicKey, payloadKey SymmetricKey, index uint64) receiverKeys {
	// Derive a message-specific shared secret by hashing the symmetric key and
	// the ephemeral public key together. This lets us use nonces that are
	// simple counters.
	derivedKeyDigest := sha512.New()
	derivedKeyDigest.Write([]byte(signcryptionSymmetricKeyContext))
	derivedKeyDigest.Write(ephemeralPub.ToKID())
	derivedKeyDigest.Write(receiverSymmetricKey.Key[:])
	derivedKey, err := rawBoxKeyFromSlice(derivedKeyDigest.Sum(nil)[0:32])
	if err != nil {
		panic(err) // should be statically impossible, if the slice above is the right length
	}

	payloadKeyBox := secretbox.Seal(
		nil,
		payloadKey[:],
		(*[24]byte)(nonceForPayloadKeyBoxV2(index)),
		(*[32]byte)(derivedKey))

	// Unlike the box key case, the identifier is supplied by the caller rather
	// than computed. (These will be KBFS TLF pseudonyms.)
	return receiverKeys{
		ReceiverKID:   receiverSymmetricKey.Identifier,
		PayloadKeyBox: payloadKeyBox,
	}
}

func (sss *signcryptSealStream) init(receiverBoxKeys []BoxPublicKey, receiverSymmetricKeys []ReceiverSymmetricKey) error {
	ephemeralKey, err := sss.keyring.CreateEphemeralKey()
	if err != nil {
		return err
	}

	eh := &SigncryptionHeader{
		FormatName: SaltpackFormatName,
		Version:    SaltpackVersion2,
		Type:       MessageTypeSigncryption,
		Ephemeral:  ephemeralKey.GetPublicKey().ToKID(),
	}
	sss.header = eh
	if err := randomFill(sss.encryptionKey[:]); err != nil {
		return err
	}

	// TODO: anonymous senders?
	if sss.signingKey == nil {
		panic("TODO: anonymous senders")
	}
	eh.SenderSecretbox = secretbox.Seal([]byte{}, sss.signingKey.GetPublicKey().ToKID(), (*[24]byte)(nonceForSenderKeySecretBox()), (*[32]byte)(&sss.encryptionKey))

	var recipientIndex uint64
	for _, receiverBoxKey := range receiverBoxKeys {
		eh.Receivers = append(eh.Receivers, receiverEntryForBoxKey(receiverBoxKey, ephemeralKey, sss.encryptionKey, recipientIndex))
		recipientIndex++
	}
	for _, receiverSymmetricKey := range receiverSymmetricKeys {
		eh.Receivers = append(eh.Receivers, receiverEntryForSymmetricKey(receiverSymmetricKey, ephemeralKey.GetPublicKey(), sss.encryptionKey, recipientIndex))
		recipientIndex++
	}

	// Encode the header to bytes, hash it, then double encode it.
	headerBytes, err := encodeToBytes(sss.header)
	if err != nil {
		return err
	}
	headerHash := sha512.Sum512(headerBytes)
	sss.headerHash = headerHash[:]
	err = sss.encoder.Encode(headerBytes)
	if err != nil {
		return err
	}

	return nil
}

func (sss *signcryptSealStream) Close() error {
	for sss.buffer.Len() > 0 {
		err := sss.signcryptBlock()
		if err != nil {
			return err
		}
	}
	return sss.writeFooter()
}

func (sss *signcryptSealStream) writeFooter() error {
	return sss.signcryptBytes([]byte{})
}

// NewSigncryptSealStream creates a stream that consumes plaintext data. It
// will write out signed and encrypted data to the io.Writer passed in as
// ciphertext. The encryption is from the specified sender, and is encrypted
// for the given receivers.
//
// Returns an io.WriteClose that accepts plaintext data to be signcrypted; and
// also returns an error if initialization failed.
func NewSigncryptSealStream(ciphertext io.Writer, keyring Keyring, sender SigningSecretKey, receiverBoxKeys []BoxPublicKey, receiverSymmetricKeys []ReceiverSymmetricKey) (io.WriteCloser, error) {
	sss := &signcryptSealStream{
		output:     ciphertext,
		encoder:    newEncoder(ciphertext),
		inblock:    make([]byte, encryptionBlockSize),
		signingKey: sender,
		keyring:    keyring,
	}
	err := sss.init(receiverBoxKeys, receiverSymmetricKeys)
	return sss, err
}

// Seal a plaintext from the given sender, for the specified receiver groups.
// Returns a ciphertext, or an error if something bad happened.
func SigncryptSeal(plaintext []byte, keyring Keyring, sender SigningSecretKey, receiverBoxKeys []BoxPublicKey, receiverSymmetricKeys []ReceiverSymmetricKey) (out []byte, err error) {
	var buf bytes.Buffer
	sss, err := NewSigncryptSealStream(&buf, keyring, sender, receiverBoxKeys, receiverSymmetricKeys)
	if err != nil {
		return nil, err
	}
	if _, err := sss.Write(plaintext); err != nil {
		return nil, err
	}
	if err := sss.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}
