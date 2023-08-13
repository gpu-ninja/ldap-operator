/* SPDX-License-Identifier: Apache-2.0
 *
 * Copyright 2023 Damian Peckett <damian@pecke.tt>.
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 * http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

package main

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"github.com/fatih/color"
	"go.uber.org/zap"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/tools/clientcmd"
)

var (
	red   = color.New(color.FgRed).SprintFunc()
	green = color.New(color.FgGreen).SprintFunc()
)

func main() {
	logger, err := zap.NewDevelopment()
	if err != nil {
		panic(err)
	}

	pwd, err := os.Getwd()
	if err != nil {
		logger.Fatal(red("Failed to get current working directory"), zap.Error(err))
	}

	logger.Info("Building operator image")

	buildContextPath := filepath.Clean(filepath.Join(pwd, ".."))

	imageName := "ghcr.io/gpu-ninja/openldap-operator:latest-dev"
	if err := buildOperatorImage(buildContextPath, "Dockerfile", imageName); err != nil {
		logger.Fatal(red("Failed to build operator image"), zap.Error(err))
	}

	logger.Info("Creating k3d cluster")

	clusterName := "openldap-operator-test"
	if err := createK3dCluster(clusterName); err != nil {
		logger.Fatal(red("Failed to create k3d cluster"), zap.Error(err))
	}
	defer func() {
		logger.Info("Deleting k3d cluster")

		if err := deleteK3dCluster(clusterName); err != nil {
			logger.Fatal(red("Failed to delete k3d cluster"), zap.Error(err))
		}
	}()

	logger.Info("Loading operator image into k3d cluster")

	if err := loadOperatorImage(clusterName, imageName); err != nil {
		logger.Fatal(red("Failed to load operator image"), zap.Error(err))
	}

	logger.Info("Installing cert-manager and operator")

	certManagerVersion := "v1.12.0"
	if err := installCertManager(certManagerVersion); err != nil {
		logger.Fatal(red("Failed to install cert-manager"), zap.Error(err))
	}

	overrideYAMLPath := filepath.Join(pwd, "config/dev.yaml")
	if err := installOperator(overrideYAMLPath, filepath.Join(pwd, "../config")); err != nil {
		logger.Fatal(red("Failed to install operator"), zap.Error(err))
	}

	logger.Info("Creating example resources")

	if err := createExampleResources(filepath.Join(pwd, "../examples")); err != nil {
		logger.Fatal(red("Failed to create example resources"), zap.Error(err))
	}

	kubeconfig := filepath.Join(clientcmd.RecommendedConfigDir, "config")
	config, err := clientcmd.BuildConfigFromFlags("", kubeconfig)
	if err != nil {
		logger.Fatal(red("Failed to build kubeconfig"), zap.Error(err))
	}

	dynClient, err := dynamic.NewForConfig(config)
	if err != nil {
		logger.Fatal(red("Failed to create kubernetes client"), zap.Error(err))
	}

	userGVR := schema.GroupVersionResource{
		Group:    "openldap.gpu-ninja.com",
		Version:  "v1alpha1",
		Resource: "ldapusers",
	}

	groupGVR := schema.GroupVersionResource{
		Group:    "openldap.gpu-ninja.com",
		Version:  "v1alpha1",
		Resource: "ldapgroups",
	}

	logger.Info("Waiting for LDAPUser and LDAPGroup to be created")

	ctx := context.Background()
	err = wait.PollUntilContextTimeout(ctx, 5*time.Second, 5*time.Minute, true, func(ctx context.Context) (bool, error) {
		ldapUser, err := dynClient.Resource(userGVR).Namespace("default").Get(ctx, "demo", metav1.GetOptions{})
		if err != nil {
			if !apierrors.IsNotFound(err) {
				return true, err
			}

			return false, nil
		}

		// select the status.phase field
		phase, ok, err := unstructured.NestedString(ldapUser.Object, "status", "phase")
		if err != nil {
			return false, err
		}

		if !ok || phase != "Ready" {
			logger.Info("LDAPUser not ready")

			return false, nil
		}

		ldapGroup, err := dynClient.Resource(groupGVR).Namespace("default").Get(ctx, "admins", metav1.GetOptions{})
		if err != nil {
			if !apierrors.IsNotFound(err) {
				return true, err
			}

			return false, nil
		}

		phase, ok, err = unstructured.NestedString(ldapGroup.Object, "status", "phase")
		if err != nil {
			return false, err
		}

		if !ok || phase != "Ready" {
			logger.Info("LDAPGroup not ready")

			return false, nil
		}

		return true, nil
	})
	if err != nil {
		logger.Fatal(red("Failed to wait for LDAPUser and LDAPGroup to be created"), zap.Error(err))
	}

	logger.Info(green("LDAPUser and LDAPGroup created successfully"))
}

func buildOperatorImage(buildContextPath, relDockerfilePath, image string) error {
	cmd := exec.Command("docker", "build", "-t", image, "-f",
		filepath.Join(buildContextPath, relDockerfilePath), buildContextPath)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	return cmd.Run()
}

func createK3dCluster(clusterName string) error {
	cmd := exec.Command("k3d", "cluster", "create", clusterName, "--wait")
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	return cmd.Run()
}

func deleteK3dCluster(clusterName string) error {
	cmd := exec.Command("k3d", "cluster", "delete", clusterName)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	return cmd.Run()
}

func loadOperatorImage(clusterName, imageName string) error {
	cmd := exec.Command("k3d", "image", "import", "-c", clusterName, imageName)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	return cmd.Run()
}

func installCertManager(certManagerVersion string) error {
	cmd := exec.Command("kapp", "deploy", "-y", "-a", "cert-manager", "-f",
		fmt.Sprintf("https://github.com/cert-manager/cert-manager/releases/download/%s/cert-manager.yaml", certManagerVersion))
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	return cmd.Run()
}

func installOperator(overrideYAMLPath, configDir string) error {
	cmd := exec.Command("ytt", "-f", overrideYAMLPath, "-f", configDir)
	patchedYAML, err := cmd.CombinedOutput()
	if err != nil {
		return err
	}

	cmd = exec.Command("kapp", "deploy", "-y", "-a", "openldap-operator", "-f", "-")
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Stdin = bytes.NewReader(patchedYAML)

	return cmd.Run()
}

func createExampleResources(examplesDir string) error {
	cmd := exec.Command("kapp", "deploy", "-y", "-a", "openldap-operator-examples", "-f", examplesDir)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	return cmd.Run()
}