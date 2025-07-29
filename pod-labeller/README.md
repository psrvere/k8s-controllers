# Pod Labeler Controller

A Kubernetes controller that automatically labels newly created Pods based on their metadata. This controller watches for Pod creation events in a specific namespace and applies labels based on the Pod's metadata (e.g., labeling based on the app name).

## Overview

This controller mimics real-world auto-tagging functionality commonly used in CI/CD pipelines for resource organization and management.

## Architecture

The controller follows the standard Kubernetes controller pattern:
- Watches Pod resources in a specific namespace
- Filters for newly created Pods
- Applies labels based on metadata
- Reconciles state changes