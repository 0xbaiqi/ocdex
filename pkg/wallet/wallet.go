package wallet

import (
	"crypto/ecdsa"
	"errors"
	"strings"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/crypto"
	hdwallet "github.com/miguelmota/go-ethereum-hdwallet"
	"github.com/tyler-smith/go-bip39"
)

type WalletManager struct {
	Mnemonic   string
	PrivateKey *ecdsa.PrivateKey
	Address    common.Address
}

// NewWalletManager initializes the wallet. If mnemonic is empty, it generates a new one.
func NewWalletManager(mnemonic string) (*WalletManager, error) {
	wm := &WalletManager{}

	if mnemonic == "" {
		// Generate new mnemonic
		entropy, err := bip39.NewEntropy(128) // 12 phrase
		if err != nil {
			return nil, err
		}
		newMnemonic, err := bip39.NewMnemonic(entropy)
		if err != nil {
			return nil, err
		}
		wm.Mnemonic = newMnemonic
	} else {
		wm.Mnemonic = mnemonic
	}

	// Validate Mnemonic
	if !bip39.IsMnemonicValid(wm.Mnemonic) {
		return nil, errors.New("无效的助记词")
	}

	// Derive Private Key and Address
	if err := wm.deriveAccount(); err != nil {
		return nil, err
	}

	return wm, nil
}

func (wm *WalletManager) deriveAccount() error {
	// Use go-ethereum-hdwallet for standard derivation path
	wallet, err := hdwallet.NewFromMnemonic(wm.Mnemonic)
	if err != nil {
		return err
	}

	path := hdwallet.MustParseDerivationPath("m/44'/60'/0'/0/0")
	account, err := wallet.Derive(path, false)
	if err != nil {
		return err
	}

	// Get Private Key
	pkStr, err := wallet.PrivateKeyHex(account)
	if err != nil {
		return err
	}
	// Remove 0x prefix if present
	pkStr = strings.TrimPrefix(pkStr, "0x")

	privateKey, err := crypto.HexToECDSA(pkStr)
	if err != nil {
		return err
	}

	wm.PrivateKey = privateKey
	wm.Address = crypto.PubkeyToAddress(privateKey.PublicKey)

	return nil
}

func (wm *WalletManager) GetAddress() string {
	return wm.Address.Hex()
}

func (wm *WalletManager) GetPrivateKeyHex() string {
	return hexutil.Encode(crypto.FromECDSA(wm.PrivateKey))
}
