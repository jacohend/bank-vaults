package main

import (
	"github.com/hashicorp/vault/api"
	"github.com/jacohend/bank-vaults/pkg/vault"
	"github.com/sirupsen/logrus"
	"github.com/spf13/cobra"
	"os"
	"time"
)

const cfgUnsealPeriod = "unseal-period"
const cfgInit = "init"

type unsealCfg struct {
	unsealPeriod time.Duration
	proceedInit  bool
}

var unsealConfig unsealCfg

var unsealCmd = &cobra.Command{
	Use:   "unseal",
	Short: "Unseals Vault with with unseal keys stored in one of the supported Cloud Provider options.",
	Long: `It will continuously attempt to unseal the target Vault instance, by retrieving unseal keys
from one of the followings:
- Google Cloud KMS keyring (backed by GCS)
- AWS KMS keyring (backed by S3)
- Azure Key Vault
- Kubernetes Secrets (should be used only for development purposes)`,
	Run: func(cmd *cobra.Command, args []string) {
		appConfig.BindPFlag(cfgUnsealPeriod, cmd.PersistentFlags().Lookup(cfgUnsealPeriod))
		appConfig.BindPFlag(cfgInit, cmd.PersistentFlags().Lookup(cfgInit))
		appConfig.BindPFlag(cfgInitRootToken, cmd.PersistentFlags().Lookup(cfgInitRootToken))
		appConfig.BindPFlag(cfgStoreRootToken, cmd.PersistentFlags().Lookup(cfgStoreRootToken))
		unsealConfig.unsealPeriod = appConfig.GetDuration(cfgUnsealPeriod)
		unsealConfig.proceedInit = appConfig.GetBool(cfgInit)

		store, err := kvStoreForConfig(appConfig)

		if err != nil {
			logrus.Fatalf("error creating kv store: %s", err.Error())
		}

		cl, err := api.NewClient(nil)

		if err != nil {
			logrus.Fatalf("error connecting to vault: %s", err.Error())
		}

		vaultConfig, err := vaultConfigForConfig(appConfig)

		if err != nil {
			logrus.Fatalf("error building vault config: %s", err.Error())
		}

		v, err := vault.New(store, cl, vaultConfig)

		if err != nil {
			logrus.Fatalf("error creating vault helper: %s", err.Error())
		}

		for i := 0; i <= 3; i++ {
			func() {
				if unsealConfig.proceedInit {
					logrus.Infof("initializing vault...")
					if err = v.Init(); err != nil {
						logrus.Fatalf("error initializing vault: %s", err.Error())
						os.Exit(1)
					} else {
						unsealConfig.proceedInit = false
					}
				}

				logrus.Infof("checking if vault is sealed...")
				sealed, err := v.Sealed()
				if err != nil {
					logrus.Errorf("error checking if vault is sealed: %s", err.Error())
					os.Exit(1)
				}

				logrus.Infof("vault sealed: %t", sealed)

				// If vault is not sealed, we stop here and wait another unsealPeriod
				if !sealed {
					os.Exit(0)
				}

				if err = v.Unseal(); err != nil {
					logrus.Errorf("error unsealing vault: %s", err.Error())
					os.Exit(1)
				}

				logrus.Infof("successfully unsealed vault")
			}()
			// wait unsealPeriod before trying again
			time.Sleep(unsealConfig.unsealPeriod)
		}
		os.Exit(0)
	},
}

func init() {
	unsealCmd.PersistentFlags().Duration(cfgUnsealPeriod, time.Second*30, "How often to attempt to unseal the vault instance")
	unsealCmd.PersistentFlags().Bool(cfgInit, false, "Initialize vault instantce if not yet initialized")
	unsealCmd.PersistentFlags().String(cfgInitRootToken, "", "root token for the new vault cluster (only if -init=true)")
	unsealCmd.PersistentFlags().Bool(cfgStoreRootToken, true, "should the root token be stored in the key store (only if -init=true)")

	rootCmd.AddCommand(unsealCmd)
}
