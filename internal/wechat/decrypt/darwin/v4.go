package darwin

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/sha512"
	"encoding/binary"
	"encoding/hex"
	"hash"
	"io"
	"os"

	"github.com/sjzar/chatlog/internal/errors"
	"github.com/sjzar/chatlog/internal/wechat/decrypt/common"
	"golang.org/x/crypto/pbkdf2"
)

const (
	PageSize     = 4096
	ReserveSize  = 80 // IV(16) + HMAC(64)
	SaltSize     = 16
	AESBlockSize = 16
	KDFIter      = 256000
	KDFIterMac   = 2
	HMACSize     = 64
)

type V4Decryptor struct{}

func NewV4Decryptor() *V4Decryptor {
	return &V4Decryptor{}
}

func (d *V4Decryptor) Decrypt(ctx context.Context, dbfile string, hexKey string, output io.Writer) error {
	key, err := hex.DecodeString(hexKey)
	if err != nil {
		return errors.DecodeKeyFailed(err)
	}
	if len(key) != common.KeySize {
		return errors.ErrKeyLengthMust32
	}

	dbFile, err := os.Open(dbfile)
	if err != nil {
		return errors.OpenFileFailed(dbfile, err)
	}
	defer dbFile.Close()

	fileInfo, err := dbFile.Stat()
	if err != nil {
		return errors.StatFileFailed(dbfile, err)
	}
	totalPages := int((fileInfo.Size() + PageSize - 1) / PageSize)
	if totalPages <= 0 {
		return errors.ErrDecryptIncorrectKey
	}

	// Read salt from first 16 bytes of the database file
	salt := make([]byte, SaltSize)
	if _, err := io.ReadFull(dbFile, salt); err != nil {
		return errors.ReadFileFailed(dbfile, err)
	}
	// Seek back to beginning
	if _, err := dbFile.Seek(0, io.SeekStart); err != nil {
		return errors.ReadFileFailed(dbfile, err)
	}

	// Derive encryption key and MAC key using PBKDF2 (SQLCipher 4 standard)
	encKey, macKey := deriveKeys(key, salt)

	page := make([]byte, PageSize)
	for pg := 0; pg < totalPages; pg++ {
		select {
		case <-ctx.Done():
			return errors.ErrDecryptOperationCanceled
		default:
		}

		n, readErr := io.ReadFull(dbFile, page)
		if readErr != nil {
			if readErr == io.EOF || readErr == io.ErrUnexpectedEOF {
				if n == 0 {
					break
				}
				for i := n; i < PageSize; i++ {
					page[i] = 0
				}
			} else {
				return errors.ReadFileFailed(dbfile, readErr)
			}
		}

		dec, err := decryptPage(page, encKey, macKey, pg+1)
		if err != nil {
			return err
		}
		if _, err := output.Write(dec); err != nil {
			return errors.WriteOutputFailed(err)
		}
	}

	return nil
}

func (d *V4Decryptor) Validate(page1 []byte, key []byte) bool {
	if len(page1) < PageSize || len(key) != common.KeySize {
		return false
	}
	salt := page1[:SaltSize]
	encKey, macKey := deriveKeys(key, salt)
	dec, err := decryptPage(page1[:PageSize], encKey, macKey, 1)
	if err != nil || len(dec) < len(common.SQLiteHeader) {
		return false
	}
	return string(dec[:len(common.SQLiteHeader)]) == common.SQLiteHeader
}

func (d *V4Decryptor) GetPageSize() int {
	return PageSize
}

func (d *V4Decryptor) GetReserve() int {
	return ReserveSize
}

func (d *V4Decryptor) GetHMACSize() int {
	return HMACSize
}

func (d *V4Decryptor) GetHashFunc() func() hash.Hash {
	return sha512.New
}

func (d *V4Decryptor) DeriveKeys(key []byte, salt []byte) ([]byte, []byte, error) {
	if len(key) != common.KeySize {
		return nil, nil, errors.ErrKeyLengthMust32
	}
	encKey, macKey := deriveKeys(key, salt)
	return encKey, macKey, nil
}

func (d *V4Decryptor) GetVersion() string {
	return "Darwin v4 (SQLCipher 4 compatible)"
}

// deriveKeys derives the encryption key and MAC key from the passphrase and salt
// using SQLCipher 4's standard PBKDF2-HMAC-SHA512 key derivation.
func deriveKeys(passphrase, salt []byte) (encKey, macKey []byte) {
	// Step 1: Derive encryption key
	// enc_key = PBKDF2-HMAC-SHA512(passphrase, salt, 256000, 32)
	encKey = pbkdf2.Key(passphrase, salt, KDFIter, 32, sha512.New)

	// Step 2: Derive MAC salt by XORing each byte of salt with 0x3a
	macSalt := make([]byte, len(salt))
	for i := range salt {
		macSalt[i] = salt[i] ^ 0x3a
	}

	// Step 3: Derive MAC key
	// mac_key = PBKDF2-HMAC-SHA512(enc_key, mac_salt, 2, 32)
	macKey = pbkdf2.Key(encKey, macSalt, KDFIterMac, 32, sha512.New)

	return encKey, macKey
}

// verifyPageHMAC verifies the HMAC-SHA512 of a page.
// SQLCipher 4 HMAC covers: encrypted_data (excluding salt on page 1) + IV + page_number (4 bytes LE)
func verifyPageHMAC(pageData, macKey []byte, pgno int) bool {
	if len(pageData) < PageSize || len(macKey) != 32 {
		return false
	}

	mac := hmac.New(sha512.New, macKey)

	// Data to HMAC: from salt end to IV end (encrypted data + IV)
	// For page 1: skip salt (first 16 bytes)
	// For other pages: start from byte 0
	offset := 0
	if pgno == 1 {
		offset = SaltSize
	}
	// HMAC covers: pageData[offset : PageSize-ReserveSize+IVSize]
	// = pageData[offset : 4016+16] = pageData[offset : 4032]
	dataEnd := PageSize - ReserveSize + IVSize // = 4016 + 16 = 4032
	mac.Write(pageData[offset:dataEnd])

	// Append page number as 4-byte little-endian
	pageNoBytes := make([]byte, 4)
	binary.LittleEndian.PutUint32(pageNoBytes, uint32(pgno))
	mac.Write(pageNoBytes)

	calculatedMAC := mac.Sum(nil)

	// Stored HMAC is at: PageSize - ReserveSize + IVSize to PageSize - ReserveSize + IVSize + HMACSize
	// = 4032 to 4032+64 = 4096
	storedMAC := pageData[dataEnd : dataEnd+HMACSize]

	return hmac.Equal(calculatedMAC[:HMACSize], storedMAC)
}

func decryptPage(pageData, encKey, macKey []byte, pgno int) ([]byte, error) {
	if len(pageData) < PageSize || len(encKey) != common.KeySize {
		return nil, errors.ErrDecryptIncorrectKey
	}

	// Verify HMAC before decryption
	if len(macKey) == 32 {
		if !verifyPageHMAC(pageData, macKey, pgno) {
			return nil, errors.ErrDecryptHashVerificationFailed
		}
	}

	ivOffset := PageSize - ReserveSize
	iv := pageData[ivOffset : ivOffset+16]

	result := make([]byte, PageSize)

	if pgno == 1 {
		enc := pageData[SaltSize : PageSize-ReserveSize]
		dec, err := aesCBCDecrypt(encKey, iv, enc)
		if err != nil {
			return nil, err
		}
		copy(result[:16], []byte(common.SQLiteHeader))
		copy(result[16:PageSize-ReserveSize], dec)
		return result, nil
	}

	enc := pageData[:PageSize-ReserveSize]
	dec, err := aesCBCDecrypt(encKey, iv, enc)
	if err != nil {
		return nil, err
	}
	copy(result[:PageSize-ReserveSize], dec)
	return result, nil
}

func aesCBCDecrypt(key []byte, iv []byte, data []byte) ([]byte, error) {
	if len(data) == 0 || len(data)%AESBlockSize != 0 {
		return nil, errors.ErrDecryptIncorrectKey
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, errors.DecryptCreateCipherFailed(err)
	}
	out := make([]byte, len(data))
	copy(out, data)
	mode := cipher.NewCBCDecrypter(block, iv)
	mode.CryptBlocks(out, out)
	return out, nil
}
