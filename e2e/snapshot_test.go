package e2e

import (
	_ "embed"
	"fmt"
	"strings"

	snapapi "github.com/kubernetes-csi/external-snapshotter/client/v6/apis/volumesnapshot/v1"
	. "github.com/onsi/ginkgo/v2"
	"github.com/onsi/ginkgo/v2/types"
	. "github.com/onsi/gomega"
	"github.com/topolvm/topolvm"
	corev1 "k8s.io/api/core/v1"
)

var (
	//go:embed testdata/snapshot_restore/snapshot-template.yaml
	thinSnapshotTemplateYAML string

	//go:embed testdata/snapshot_restore/restore-pvc-template.yaml
	thinRestorePVCTemplateYAML string

	//go:embed testdata/snapshot_restore/restore-pod-template.yaml
	thinRestorePodTemplateYAML string
)

const (
	volName        = "thinvol"
	snapName       = "thinsnap"
	restorePVCName = "thinrestore"
	restorePodName = "thin-restore-pod"
	// size of PVC in GBs
	pvcSize = "1"
)

func testSnapRestore() {
	var nsSnapTest string
	var snapshot snapapi.VolumeSnapshot

	BeforeEach(func() {
		nsSnapTest = "snap-test-" + randomString(10)
		createNamespace(nsSnapTest)
	})
	AfterEach(func() {
		if !CurrentSpecReport().State.Is(types.SpecStateFailureStates) {
			kubectl("delete", "namespaces/"+nsSnapTest)
		}
	})

	It("should create a thin-snap", func() {
		By("deploying Pod with PVC")

		nodeName := "topolvm-e2e-worker"
		if isDaemonsetLvmdEnvSet() {
			nodeName = getDaemonsetLvmdNodeName()
		}
		thinPvcYAML := []byte(fmt.Sprintf(thinPVCTemplateYAML, volName, pvcSize))
		_, err := kubectlWithInput(thinPvcYAML, "apply", "-n", nsSnapTest, "-f", "-")
		Expect(err).ShouldNot(HaveOccurred())

		thinPodYAML := []byte(fmt.Sprintf(thinPodTemplateYAML, "thinpod", volName, topolvm.GetTopologyNodeKey(), nodeName))
		_, err = kubectlWithInput(thinPodYAML, "apply", "-n", nsSnapTest, "-f", "-")
		Expect(err).ShouldNot(HaveOccurred())

		By("confirming if the resources have been created")
		Eventually(func() error {
			var pvc corev1.PersistentVolumeClaim
			err := getObjects(&pvc, "pvc", "-n", nsSnapTest, volName)
			if err != nil {
				return fmt.Errorf("failed to get PVC. err: %w", err)
			}
			if pvc.Status.Phase != corev1.ClaimBound {
				return fmt.Errorf("PVC %s is not bound", volName)
			}
			return nil
		}).Should(Succeed())

		By("writing file under /test1")
		writePath := "/test1/bootstrap.log"
		Eventually(func() error {
			_, err = kubectl("exec", "-n", nsSnapTest, "thinpod", "--", "cp", "/var/log/bootstrap.log", writePath)
			return err
		}).Should(Succeed())

		_, err = kubectl("exec", "-n", nsSnapTest, "thinpod", "--", "sync")
		Expect(err).ShouldNot(HaveOccurred())
		stdout, err := kubectl("exec", "-n", nsSnapTest, "thinpod", "--", "cat", writePath)
		Expect(err).ShouldNot(HaveOccurred())
		Expect(strings.TrimSpace(string(stdout))).ShouldNot(BeEmpty())

		By("creating a snap")
		thinSnapshotYAML := []byte(fmt.Sprintf(thinSnapshotTemplateYAML, snapName, "topolvm-provisioner-thin", "thinvol"))
		_, err = kubectlWithInput(thinSnapshotYAML, "apply", "-n", nsSnapTest, "-f", "-")
		Expect(err).ShouldNot(HaveOccurred())

		Eventually(func() error {
			err := getObjects(&snapshot, "vs", snapName, "-n", nsSnapTest)
			if err != nil {
				return fmt.Errorf("failed to get VolumeSnapshot. err: %w", err)
			}
			if snapshot.Status == nil {
				return fmt.Errorf("waiting for snapshot status")
			}
			if *snapshot.Status.ReadyToUse != true {
				return fmt.Errorf("Snapshot is not Ready To Use")
			}
			return nil
		}).Should(Succeed())

		By("restoring the snap")
		thinPVCRestoreYAML := []byte(fmt.Sprintf(thinRestorePVCTemplateYAML, restorePVCName, pvcSize, snapName))
		_, err = kubectlWithInput(thinPVCRestoreYAML, "apply", "-n", nsSnapTest, "-f", "-")
		Expect(err).ShouldNot(HaveOccurred())

		thinPVCRestorePodYAML := []byte(fmt.Sprintf(thinRestorePodTemplateYAML, restorePodName, restorePVCName, topolvm.GetTopologyNodeKey(), nodeName))
		_, err = kubectlWithInput(thinPVCRestorePodYAML, "apply", "-n", nsSnapTest, "-f", "-")
		Expect(err).ShouldNot(HaveOccurred())

		By("verifying if the restored PVC is created")
		Eventually(func() error {
			var pvc corev1.PersistentVolumeClaim
			err := getObjects(&pvc, "pvc", "-n", nsSnapTest, restorePVCName)
			if err != nil {
				return fmt.Errorf("failed to get PVC. err: %w", err)
			}
			if pvc.Status.Phase != corev1.ClaimBound {
				return fmt.Errorf("PVC %s is not bound", restorePVCName)
			}
			return nil
		}).Should(Succeed())

		var lvName string
		Eventually(func() error {
			lvName, err = getLVNameOfPVC(restorePVCName, nsSnapTest)
			return err
		}).Should(Succeed())

		var lv *lvinfo
		Eventually(func() error {
			lv, err = getLVInfo(lvName)
			return err
		}).Should(Succeed())

		vgName := "node1-myvg4"
		if isDaemonsetLvmdEnvSet() {
			vgName = "node-myvg5"
		}
		Expect(vgName).Should(Equal(lv.vgName))

		poolName := "pool0"
		Expect(poolName).Should(Equal(lv.poolName))

		By("confirming that the file exists")
		Eventually(func() error {
			stdout, err = kubectl("exec", "-n", nsSnapTest, restorePodName, "--", "cat", writePath)
			if err != nil {
				return fmt.Errorf("failed to cat. err: %w", err)
			}
			if len(strings.TrimSpace(string(stdout))) == 0 {
				return fmt.Errorf(writePath + " is empty")
			}
			return nil
		}).Should(Succeed())

	})

	It("validating if the restored PVCs are standalone", func() {
		By("deleting the source PVC")

		nodeName := "topolvm-e2e-worker"
		if isDaemonsetLvmdEnvSet() {
			nodeName = getDaemonsetLvmdNodeName()
		}

		By("creating a PVC and application")
		thinPvcYAML := []byte(fmt.Sprintf(thinPVCTemplateYAML, volName, pvcSize))
		_, err := kubectlWithInput(thinPvcYAML, "apply", "-n", nsSnapTest, "-f", "-")
		Expect(err).ShouldNot(HaveOccurred())

		thinPodYAML := []byte(fmt.Sprintf(thinPodTemplateYAML, "thinpod", volName, topolvm.GetTopologyNodeKey(), nodeName))
		_, err = kubectlWithInput(thinPodYAML, "apply", "-n", nsSnapTest, "-f", "-")
		Expect(err).ShouldNot(HaveOccurred())
		By("verifying if the PVC is created")
		Eventually(func() error {
			var pvc corev1.PersistentVolumeClaim
			err := getObjects(&pvc, "pvc", "-n", nsSnapTest, volName)
			if err != nil {
				return fmt.Errorf("failed to get PVC. err: %w", err)
			}
			if pvc.Status.Phase != corev1.ClaimBound {
				return fmt.Errorf("PVC %s is not bound", volName)
			}
			return nil
		}).Should(Succeed())

		By("creating a snap of the PVC")
		thinSnapshotYAML := []byte(fmt.Sprintf(thinSnapshotTemplateYAML, snapName, "topolvm-provisioner-thin", "thinvol"))
		_, err = kubectlWithInput(thinSnapshotYAML, "apply", "-n", nsSnapTest, "-f", "-")
		Expect(err).ShouldNot(HaveOccurred())
		Eventually(func() error {
			err := getObjects(&snapshot, "vs", snapName, "-n", nsSnapTest)
			if err != nil {
				return fmt.Errorf("failed to get VolumeSnapshot. err: %w", err)
			}
			if snapshot.Status == nil {
				return fmt.Errorf("waiting for snapshot status")
			}
			if *snapshot.Status.ReadyToUse != true {
				return fmt.Errorf("Snapshot is not Ready To Use")
			}
			return nil
		}).Should(Succeed())

		By("restoring the snap")
		thinPVCRestoreYAML := []byte(fmt.Sprintf(thinRestorePVCTemplateYAML, restorePVCName, pvcSize, snapName))
		_, err = kubectlWithInput(thinPVCRestoreYAML, "apply", "-n", nsSnapTest, "-f", "-")
		Expect(err).ShouldNot(HaveOccurred())

		thinPVCRestorePodYAML := []byte(fmt.Sprintf(thinRestorePodTemplateYAML, restorePodName, restorePVCName, topolvm.GetTopologyNodeKey(), nodeName))
		_, err = kubectlWithInput(thinPVCRestorePodYAML, "apply", "-n", nsSnapTest, "-f", "-")
		Expect(err).ShouldNot(HaveOccurred())

		By("verifying if the restored PVC is created")
		Eventually(func() error {
			var pvc corev1.PersistentVolumeClaim
			err := getObjects(&pvc, "pvc", "-n", nsSnapTest, restorePVCName)
			if err != nil {
				return fmt.Errorf("failed to get PVC. err: %w", err)
			}
			if pvc.Status.Phase != corev1.ClaimBound {
				return fmt.Errorf("PVC %s is not bound", restorePVCName)
			}
			return nil
		}).Should(Succeed())

		By("validating if the restored volume is present")
		var lvName string
		Eventually(func() error {
			lvName, err = getLVNameOfPVC(restorePVCName, nsSnapTest)
			return err
		}).Should(Succeed())

		var lv *lvinfo
		Eventually(func() error {
			lv, err = getLVInfo(lvName)
			return err
		}).Should(Succeed())

		vgName := "node1-myvg4"
		if isDaemonsetLvmdEnvSet() {
			vgName = "node-myvg5"
		}
		Expect(vgName).Should(Equal(lv.vgName))

		poolName := "pool0"
		Expect(poolName).Should(Equal(lv.poolName))

		// delete the source PVC as well as the snapshot
		By("deleting source volume and snap")
		_, err = kubectlWithInput(thinPodYAML, "delete", "-n", nsSnapTest, "-f", "-")
		Expect(err).ShouldNot(HaveOccurred())

		_, err = kubectlWithInput(thinPvcYAML, "delete", "-n", nsSnapTest, "-f", "-")
		Expect(err).ShouldNot(HaveOccurred())

		_, err = kubectlWithInput(thinSnapshotYAML, "delete", "-n", nsSnapTest, "-f", "-")
		Expect(err).ShouldNot(HaveOccurred())

		By("validating if the restored volume is present and is not deleted.")
		lvName, err = getLVNameOfPVC(restorePVCName, nsSnapTest)
		Expect(err).Should(Succeed())

		_, err = getLVInfo(lvName)
		Expect(err).Should(Succeed())
	})
}
