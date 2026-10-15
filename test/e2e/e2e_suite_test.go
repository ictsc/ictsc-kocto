//go:build e2e
// +build e2e

/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package e2e

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/ictsc/ictsc-kocto/test/utils"
)

var (
	// managerImageはテスト用にビルドし、Kindへ読み込むイメージ。
	managerImage = "example.com/ictsc-kocto:v0.0.1"
	// managerManifestはconfig/managerを変更せず、一時ディレクトリへ生成する。
	managerManifest string
	// shouldCleanupCertManagerは、このテストでcert-managerを導入したかを記録する。
	shouldCleanupCertManager = false
)

// TestE2Eは使い捨てのKindクラスタでE2Eを実行する。
// 既定ではKindとcert-managerを使う。
//
// kubercを利用する場合はKUBECTL_KUBERC=trueを指定する。
// 環境による差を避けるため、既定ではkubercを無効にする。
// cert-managerの導入を省略する場合はCERT_MANAGER_INSTALL_SKIP=trueを指定する。
func TestE2E(t *testing.T) {
	// 現在のcontextを使わず、指定のKindクラスタのkubeconfigを取得する。
	// 専用のkubeconfigを使い、子プロセスも同じクラスタへ接続させる。
	cluster := os.Getenv("KIND_CLUSTER")
	if !strings.HasSuffix(cluster, "-e2e") {
		t.Fatal("Set KIND_CLUSTER to a dedicated Kind cluster name ending in -e2e")
	}
	kindBinary := os.Getenv("KIND")
	if kindBinary == "" {
		kindBinary = "kind"
	}
	config, err := exec.Command(kindBinary, "get", "kubeconfig", "--name", cluster).Output()
	if err != nil {
		t.Fatalf("Could not export kubeconfig for Kind cluster %q: %v", cluster, err)
	}
	kubeconfig := filepath.Join(t.TempDir(), "kubeconfig")
	if err := os.WriteFile(kubeconfig, config, 0o600); err != nil {
		t.Fatalf("Could not write isolated Kind kubeconfig: %v", err)
	}
	t.Setenv("KUBECONFIG", kubeconfig)

	RegisterFailHandler(Fail)
	_, _ = fmt.Fprintf(GinkgoWriter, "Starting ictsc-kocto e2e test suite\n")
	RunSpecs(t, "e2e suite")
}

var _ = BeforeSuite(func() {
	By("building the manager image")
	cmd := exec.Command("docker", "build", "-t", managerImage, ".")
	_, err := utils.Run(cmd)
	ExpectWithOffset(1, err).NotTo(HaveOccurred(), "Failed to build the manager image")

	// Kind以外へ変更する場合は、イメージの配置方法も合わせて変更する。
	By("loading the manager image on Kind")
	err = utils.LoadImageToKindClusterWithName(managerImage)
	ExpectWithOffset(1, err).NotTo(HaveOccurred(), "Failed to load the manager image into Kind")

	By("rendering the controller manifests without changing source files")
	_, err = utils.Run(exec.Command("make", "manifests", "kustomize"))
	ExpectWithOffset(1, err).NotTo(HaveOccurred(), "Failed to generate manifests")
	manifest, err := utils.Run(exec.Command("./bin/kustomize", "build", "config/default"))
	ExpectWithOffset(1, err).NotTo(HaveOccurred(), "Failed to render manifests")
	Expect(strings.Count(manifest, "image: controller:latest")).To(Equal(1))
	manifest = strings.ReplaceAll(manifest, "image: controller:latest", "image: "+managerImage)
	managerManifest = filepath.Join(GinkgoT().TempDir(), "manager.yaml")
	Expect(os.WriteFile(managerManifest, []byte(manifest), 0o600)).To(Succeed())

	configureKubectlKubeRC()
	setupCertManager()
})

var _ = AfterSuite(func() {
	teardownCertManager()
})

// 手元のkubectl設定による差を避けるため、kubercを既定で無効にする。
// 利用する場合はKUBECTL_KUBERC=trueを指定する。
func configureKubectlKubeRC() {
	if os.Getenv("KUBECTL_KUBERC") != "true" {
		By("disabling kubectl kuberc for test isolation")
		err := os.Setenv("KUBECTL_KUBERC", "false")
		ExpectWithOffset(1, err).NotTo(HaveOccurred(), "Failed to disable kubectl kuberc")
		_, _ = fmt.Fprintf(GinkgoWriter,
			"kubectl kuberc disabled for consistent test behavior (override with KUBECTL_KUBERC=true)\n")
	} else {
		_, _ = fmt.Fprintf(GinkgoWriter, "kubectl kuberc enabled (KUBECTL_KUBERC=true)\n")
	}
}

// setupCertManagerは必要な場合だけWebhookのテスト用にcert-managerを導入する。
// 導入済みかCERT_MANAGER_INSTALL_SKIP=trueの場合は省略する。
func setupCertManager() {
	if os.Getenv("CERT_MANAGER_INSTALL_SKIP") == "true" {
		_, _ = fmt.Fprintf(GinkgoWriter, "Skipping CertManager installation (CERT_MANAGER_INSTALL_SKIP=true)\n")
		return
	}

	By("checking if CertManager is already installed")
	if utils.IsCertManagerCRDsInstalled() {
		_, _ = fmt.Fprintf(GinkgoWriter, "CertManager is already installed. Skipping installation.\n")
		return
	}

	// 導入途中で失敗した場合にも片付けられるよう、先に削除対象として記録する。
	shouldCleanupCertManager = true

	By("installing CertManager")
	Expect(utils.InstallCertManager()).To(Succeed(), "Failed to install CertManager")
}

// teardownCertManagerは、このテストで導入したcert-managerだけを削除する。
func teardownCertManager() {
	if !shouldCleanupCertManager {
		_, _ = fmt.Fprintf(GinkgoWriter, "Skipping CertManager cleanup (not installed by this suite)\n")
		return
	}

	By("uninstalling CertManager")
	utils.UninstallCertManager()
}
