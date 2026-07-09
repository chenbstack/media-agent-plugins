package main

import (
	"encoding/base64"
	"fmt"
	"math/big"
)

const userAgent115 = "Mozilla/5.0 (iPhone; CPU iPhone OS 17_5_1 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) Mobile/15E148 115wangpan_ios/36.2.20"

var (
	keyL115 = []byte{0x78, 0x06, 0xad, 0x4c, 0x33, 0x86, 0x5d, 0x18, 0x4c, 0x01, 0x3f, 0x46}
	keyRSA  = []byte{0x8d, 0xa5, 0xa5, 0x8d}
	randRSA = make([]byte, 16)
	rsaN115 = mustBigInt("8686980c0f5a24c4b9d43020cd2c22703ff3f450756529058b1cf88f09b8602136477198a6e2683149659bd122c33592fdb5ad47944ad1ea4d36c6b172aad6338c3bb6ac6227502d010993ac967d1aef00f0c8e038de2e4d3bc2ec368af2e9f10a6f1eda4f7262f136420c07c331b871bf139f74f3010e3c4fe57df3afb71683")
	rsaE115 = big.NewInt(0x10001)
	gKTS115 = []byte{
		0xf0, 0xe5, 0x69, 0xae, 0xbf, 0xdc, 0xbf, 0x8a, 0x1a, 0x45, 0xe8, 0xbe, 0x7d, 0xa6, 0x73, 0xb8,
		0xde, 0x8f, 0xe7, 0xc4, 0x45, 0xda, 0x86, 0xc4, 0x9b, 0x64, 0x8b, 0x14, 0x6a, 0xb4, 0xf1, 0xaa,
		0x38, 0x01, 0x35, 0x9e, 0x26, 0x69, 0x2c, 0x86, 0x00, 0x6b, 0x4f, 0xa5, 0x36, 0x34, 0x62, 0xa6,
		0x2a, 0x96, 0x68, 0x18, 0xf2, 0x4a, 0xfd, 0xbd, 0x6b, 0x97, 0x8f, 0x4d, 0x8f, 0x89, 0x13, 0xb7,
		0x6c, 0x8e, 0x93, 0xed, 0x0e, 0x0d, 0x48, 0x3e, 0xd7, 0x2f, 0x88, 0xd8, 0xfe, 0xfe, 0x7e, 0x86,
		0x50, 0x95, 0x4f, 0xd1, 0xeb, 0x83, 0x26, 0x34, 0xdb, 0x66, 0x7b, 0x9c, 0x7e, 0x9d, 0x7a, 0x81,
		0x32, 0xea, 0xb6, 0x33, 0xde, 0x3a, 0xa9, 0x59, 0x34, 0x66, 0x3b, 0xaa, 0xba, 0x81, 0x60, 0x48,
		0xb9, 0xd5, 0x81, 0x9c, 0xf8, 0x6c, 0x84, 0x77, 0xff, 0x54, 0x78, 0x26, 0x5f, 0xbe, 0xe8, 0x1e,
		0x36, 0x9f, 0x34, 0x80, 0x5c, 0x45, 0x2c, 0x9b, 0x76, 0xd5, 0x1b, 0x8f, 0xcc, 0xc3, 0xb8, 0xf5,
	}
)

func rsaEncrypt115(data []byte) ([]byte, error) {
	tmp := reverseBytes(xor115(data, keyRSA))
	xorData := append(append([]byte{}, randRSA...), xor115(tmp, keyL115)...)
	encrypted, err := rsaEncryptWithPubKey115(xorData)
	if err != nil {
		return nil, err
	}
	out := make([]byte, base64.StdEncoding.EncodedLen(len(encrypted)))
	base64.StdEncoding.Encode(out, encrypted)
	return out, nil
}

func rsaDecrypt115(cipherData []byte) ([]byte, error) {
	decoded := make([]byte, base64.StdEncoding.DecodedLen(len(cipherData)))
	n, err := base64.StdEncoding.Decode(decoded, cipherData)
	if err != nil {
		return nil, err
	}
	decrypted, err := rsaDecryptWithPubKey115(decoded[:n])
	if err != nil {
		return nil, err
	}
	if len(decrypted) < 16 {
		return nil, fmt.Errorf("115 RSA 响应长度无效")
	}
	key := rsaGenKey115(decrypted[:16], 12)
	tmp := reverseBytes(xor115(decrypted[16:], key))
	return xor115(tmp, keyRSA), nil
}

func rsaGenKey115(randKey []byte, skLen int) []byte {
	xorKey := make([]byte, skLen)
	length := skLen * (skLen - 1)
	index := 0
	for i := range skLen {
		x := (int(randKey[i]) + int(gKTS115[index])) & 0xff
		xorKey[i] = gKTS115[length] ^ byte(x)
		length -= skLen
		index += skLen
	}
	return xorKey
}

func xor115(src, key []byte) []byte {
	if len(key) == 0 {
		return append([]byte{}, src...)
	}
	out := make([]byte, len(src))
	offset := len(src) & 3
	if offset > 0 {
		xorChunk115(out[:offset], src[:offset], key[:offset])
	}
	for offset < len(src) {
		n := len(key)
		if remaining := len(src) - offset; remaining < n {
			n = remaining
		}
		xorChunk115(out[offset:offset+n], src[offset:offset+n], key[:n])
		offset += n
	}
	return out
}

func xorChunk115(dst, a, b []byte) {
	for i := range a {
		dst[i] = a[i] ^ b[i]
	}
}

func reverseBytes(in []byte) []byte {
	out := make([]byte, len(in))
	for i := range in {
		out[len(in)-1-i] = in[i]
	}
	return out
}

func rsaEncryptWithPubKey115(data []byte) ([]byte, error) {
	var out []byte
	for start := 0; start < len(data); start += 117 {
		end := start + 117
		if end > len(data) {
			end = len(data)
		}
		padded, err := padPKCS1v15Like115(data[start:end])
		if err != nil {
			return nil, err
		}
		cipher := new(big.Int).Exp(new(big.Int).SetBytes(padded), rsaE115, rsaN115)
		out = append(out, leftPad(cipher.Bytes(), 128)...)
	}
	return out, nil
}

func rsaDecryptWithPubKey115(cipherData []byte) ([]byte, error) {
	if len(cipherData)%128 != 0 {
		return nil, fmt.Errorf("115 RSA 密文长度无效")
	}
	var out []byte
	for start := 0; start < len(cipherData); start += 128 {
		part := cipherData[start : start+128]
		plain := new(big.Int).Exp(new(big.Int).SetBytes(part), rsaE115, rsaN115).Bytes()
		idx := -1
		for i, b := range plain {
			if b == 0 {
				idx = i
				break
			}
		}
		if idx < 0 || idx+1 > len(plain) {
			return nil, fmt.Errorf("115 RSA 明文 padding 无效")
		}
		out = append(out, plain[idx+1:]...)
	}
	return out, nil
}

func padPKCS1v15Like115(message []byte) ([]byte, error) {
	if len(message) > 117 {
		return nil, fmt.Errorf("115 RSA 分片过长")
	}
	out := make([]byte, 0, 128)
	out = append(out, 0x00)
	for len(out) < 127-len(message) {
		out = append(out, 0x02)
	}
	out = append(out, 0x00)
	out = append(out, message...)
	return out, nil
}

func leftPad(in []byte, size int) []byte {
	if len(in) >= size {
		return in
	}
	out := make([]byte, size)
	copy(out[size-len(in):], in)
	return out
}

func mustBigInt(hexValue string) *big.Int {
	n, ok := new(big.Int).SetString(hexValue, 16)
	if !ok {
		panic("invalid 115 rsa modulus")
	}
	return n
}
