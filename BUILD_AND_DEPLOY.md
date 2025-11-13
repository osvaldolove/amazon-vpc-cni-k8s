# Building and Deploying the VPC-CNI Fix

## Summary

We've fixed the VPC-CNI bug that causes pods on the primary ENI to lose network connectivity during reconciliation events.

**Branch**: `fix/primary-eni-routing-bug`  
**Base Version**: v1.20.4  
**Commit**: 9a259f69

---

## What Was Fixed

### Bug Location
- **File**: `cmd/routed-eni-cni-plugin/driver/driver.go`
- **Lines**: 542 (setup) and 568 (teardown)

### The Problem
```go
// BEFORE (Buggy)
if rtTable != unix.RT_TABLE_MAIN {
    // Setup fromContainer rule
}
```

This skipped creating the `fromContainer` routing rule for pods using RT_TABLE_MAIN (254), which includes:
- Primary ENI pods in secondary IP mode
- All pods during certain reconciliation events

### The Fix
```go
// AFTER (Fixed)
// Always setup fromContainer rule for all routing tables
fromContainerRule := n.netLink.NewRule()
fromContainerRule.Src = containerAddr
fromContainerRule.Priority = networkutils.FromPodRulePriority
fromContainerRule.Table = rtTable
if err := n.netLink.RuleAdd(fromContainerRule); err != nil && !networkutils.IsRuleExistsError(err) {
    return errors.Wrapf(err, "failed to setup fromContainer rule, containerAddr=%s, rtTable=%v", containerAddr.String(), rtTable)
}
```

---

## Prerequisites

### 1. Docker
```bash
docker --version
# Should be 20.10+ or higher
```

### 2. AWS CLI (for ECR)
```bash
aws --version
# Should be 2.x
```

### 3. kubectl access to your cluster
```bash
kubectl version --short
kubectl get nodes
```

### 4. ECR Repository
```bash
# Create ECR repository if it doesn't exist
export AWS_ACCOUNT_ID=$(aws sts get-caller-identity --query Account --output text)
export AWS_REGION=eu-central-1
export ECR_REPO=amazon-k8s-cni

aws ecr create-repository \
  --repository-name $ECR_REPO \
  --region $AWS_REGION \
  2>/dev/null || echo "Repository already exists"
```

---

## Build Process

### Step 1: Verify You're on the Fix Branch

```bash
cd /home/olly/src/lcp-customer-devops/covestro/aws-ccai-infra/vpc-cni-fork
git branch
# Should show: * fix/primary-eni-routing-bug

git log --oneline -1
# Should show: 9a259f69 Fix: Setup fromContainer routing rule for all routing tables
```

### Step 2: Build the Container Image

```bash
cd /home/olly/src/lcp-customer-devops/covestro/aws-ccai-infra/vpc-cni-fork

# Build using the Makefile
make docker

# This will create:
# amazon/amazon-k8s-cni:v1.20.4
```

**Expected output**:
```
Successfully built <image-id>
Successfully tagged amazon/amazon-k8s-cni:v1.20.4
```

**Build time**: ~5-10 minutes (depending on system)

### Step 3: Tag the Image

```bash
# Set your ECR registry URL
export AWS_ACCOUNT_ID=$(aws sts get-caller-identity --query Account --output text)
export AWS_REGION=eu-central-1
export ECR_REGISTRY=$AWS_ACCOUNT_ID.dkr.ecr.$AWS_REGION.amazonaws.com

# Tag with descriptive name
docker tag amazon/amazon-k8s-cni:v1.20.4 \
  $ECR_REGISTRY/amazon-k8s-cni:v1.20.4-patched

# Also tag with date for tracking
docker tag amazon/amazon-k8s-cni:v1.20.4 \
  $ECR_REGISTRY/amazon-k8s-cni:v1.20.4-patched-$(date +%Y%m%d)

# Verify tags
docker images | grep amazon-k8s-cni
```

### Step 4: Push to ECR

```bash
# Login to ECR
aws ecr get-login-password --region $AWS_REGION | \
  docker login --username AWS --password-stdin $ECR_REGISTRY

# Push both tags
docker push $ECR_REGISTRY/amazon-k8s-cni:v1.20.4-patched
docker push $ECR_REGISTRY/amazon-k8s-cni:v1.20.4-patched-$(date +%Y%m%d)

echo "Images pushed:"
echo "  $ECR_REGISTRY/amazon-k8s-cni:v1.20.4-patched"
echo "  $ECR_REGISTRY/amazon-k8s-cni:v1.20.4-patched-$(date +%Y%m%d)"
```

---

## Deployment Process

### Before You Deploy

**IMPORTANT**: Test in a non-production environment first!

### Step 1: Backup Current Configuration

```bash
# Save current VPC-CNI DaemonSet configuration
kubectl get daemonset aws-node -n kube-system -o yaml > aws-node-backup-$(date +%Y%m%d).yaml

echo "Backup saved to: aws-node-backup-$(date +%Y%m%d).yaml"
```

### Step 2: Deploy Patched Version

```bash
# Set image URL
export AWS_ACCOUNT_ID=$(aws sts get-caller-identity --query Account --output text)
export AWS_REGION=eu-central-1
export PATCHED_IMAGE="$AWS_ACCOUNT_ID.dkr.ecr.$AWS_REGION.amazonaws.com/amazon-k8s-cni:v1.20.4-patched"

# Update the DaemonSet
kubectl set image daemonset/aws-node -n kube-system \
  aws-node=$PATCHED_IMAGE

# Watch the rollout
kubectl rollout status daemonset/aws-node -n kube-system --timeout=5m
```

**Expected output**:
```
daemonset "aws-node" image updated
Waiting for daemon set "aws-node" rollout to finish: 0 of 3 updated pods are available...
Waiting for daemon set "aws-node" rollout to finish: 1 of 3 updated pods are available...
Waiting for daemon set "aws-node" rollout to finish: 2 of 3 updated pods are available...
daemon set "aws-node" successfully rolled out
```

### Step 3: Verify Deployment

```bash
# Check all aws-node pods are running new image
kubectl get pods -n kube-system -l k8s-app=aws-node -o jsonpath='{range .items[*]}{.metadata.name}{"\t"}{.spec.containers[0].image}{"\n"}{end}'

# Should show your patched image for all pods
```

### Step 4: Test the Fix

#### A. Create a Test Pod

```bash
kubectl run test-vpc-cni-fix --image=nginx --restart=Never

# Wait for pod to start
kubectl wait --for=condition=Ready pod/test-vpc-cni-fix --timeout=60s
```

#### B. Get Pod Details

```bash
# Get pod IP and node
export TEST_POD_IP=$(kubectl get pod test-vpc-cni-fix -o jsonpath='{.status.podIP}')
export TEST_POD_NODE=$(kubectl get pod test-vpc-cni-fix -o jsonpath='{.spec.nodeName}')

echo "Pod IP: $TEST_POD_IP"
echo "Node: $TEST_POD_NODE"
```

#### C. Verify fromContainer Rule Exists

```bash
# Get aws-node pod on the same node
export AWS_NODE_POD=$(kubectl get pods -n kube-system -l k8s-app=aws-node --field-selector spec.nodeName=$TEST_POD_NODE -o jsonpath='{.items[0].metadata.name}')

echo "Checking fromContainer rule on node $TEST_POD_NODE..."

# Check if rule exists
kubectl exec -n kube-system $AWS_NODE_POD -c aws-node -- \
  ip rule list | grep "from $TEST_POD_IP"

# Should output something like:
# 512:    from 10.255.171.X lookup 254
# or
# 512:    from 10.255.171.X lookup 1
```

**If rule exists**: ✅ Fix is working!  
**If no rule**: ❌ Something went wrong

#### D. Test Pod Connectivity

```bash
# Test connection to Kubernetes API
kubectl exec test-vpc-cni-fix -- curl -s -o /dev/null -w "%{http_code}" \
  --max-time 5 https://kubernetes.default.svc.cluster.local

# Should return: 000 (can't parse HTTPS, but connection works)
# or 200-400 range (connection successful)
# 
# Should NOT timeout (exit code 28 or taking >5 seconds)
```

#### E. Test with ENI Reconciliation

**This is the critical test** - the bug manifests during reconciliation:

```bash
# Restart all VPC-CNI pods to trigger reconciliation
kubectl rollout restart daemonset/aws-node -n kube-system

# Wait for rollout
kubectl rollout status daemonset/aws-node -n kube-system --timeout=5m

# Wait additional 2 minutes for reconciliation to complete
sleep 120

# Verify rule still exists
kubectl exec -n kube-system $AWS_NODE_POD -c aws-node -- \
  ip rule list | grep "from $TEST_POD_IP"

# Verify pod still works
kubectl exec test-vpc-cni-fix -- curl -s -o /dev/null -w "%{http_code}" \
  --max-time 5 https://kubernetes.default.svc.cluster.local
```

**If rule exists AND connectivity works**: ✅ Fix survived reconciliation!  
**If rule missing OR connectivity broken**: ❌ Bug still present

#### F. Cleanup Test Pod

```bash
kubectl delete pod test-vpc-cni-fix
```

---

## Post-Deployment

### Step 1: Remove Auto-Remediation DaemonSet

Once the fix is verified working:

```bash
# The auto-remediation is no longer needed
kubectl delete daemonset vpc-cni-auto-remediation -n kube-system
kubectl delete clusterrolebinding vpc-cni-auto-remediation
kubectl delete clusterrole vpc-cni-auto-remediation
kubectl delete serviceaccount vpc-cni-auto-remediation -n kube-system

echo "Auto-remediation removed - no longer needed!"
```

### Step 2: Monitor Cluster

**For the next 24-48 hours**, monitor for:

```bash
# Check for pods in bad states
kubectl get pods -A -o wide | grep -E "CrashLoop|Error|0/[0-9]"

# Check VPC-CNI logs for errors
kubectl logs -n kube-system -l k8s-app=aws-node --tail=100 | grep -i error

# Monitor fromContainer rules on each node
for node in $(kubectl get nodes -o name | cut -d/ -f2); do
  echo "=== Node: $node ==="
  AWS_NODE_POD=$(kubectl get pods -n kube-system -l k8s-app=aws-node --field-selector spec.nodeName=$node -o jsonpath='{.items[0].metadata.name}')
  kubectl exec -n kube-system $AWS_NODE_POD -c aws-node -- \
    ip rule list | grep "from 10.255.171" | wc -l
done
```

### Step 3: Document Deployment

Update your deployment documentation:

```
Date: $(date +%Y-%m-%d)
Action: Deployed patched VPC-CNI v1.20.4 to fix primary ENI routing bug
Image: $ECR_REGISTRY/amazon-k8s-cni:v1.20.4-patched
Commit: 9a259f69
Branch: fix/primary-eni-routing-bug
Deployed by: [Your name]
Verified: [Yes/No]
```

---

## Rollback Procedure

If something goes wrong:

### Option 1: Rollback to Previous Image

```bash
# Use the backup to get original image
export ORIGINAL_IMAGE=$(grep "image:" aws-node-backup-$(date +%Y%m%d).yaml | head -1 | awk '{print $2}')

kubectl set image daemonset/aws-node -n kube-system \
  aws-node=$ORIGINAL_IMAGE

kubectl rollout status daemonset/aws-node -n kube-system
```

### Option 2: Restore from Backup

```bash
kubectl apply -f aws-node-backup-$(date +%Y%m%d).yaml
```

### Option 3: Redeploy Auto-Remediation

```bash
kubectl apply -f vpc-cni-auto-remediation-v2.yaml
```

---

## Troubleshooting

### Build Failures

**Issue**: `make docker` fails with missing dependencies

**Solution**:
```bash
# Ensure Go is installed
go version

# Ensure Make is installed
make --version

# Check Makefile requirements
cat Makefile | grep -A 5 "^docker:"
```

### Push Failures

**Issue**: Cannot push to ECR - authentication error

**Solution**:
```bash
# Re-login to ECR
aws ecr get-login-password --region $AWS_REGION | \
  docker login --username AWS --password-stdin $ECR_REGISTRY

# Verify credentials
aws sts get-caller-identity
```

### Deployment Failures

**Issue**: Pods stuck in ImagePullBackOff

**Solution**:
```bash
# Check if image exists in ECR
aws ecr describe-images \
  --repository-name amazon-k8s-cni \
  --region $AWS_REGION

# Check if nodes have ECR access
# Nodes must have IAM role with ECR permissions
```

### Fix Not Working

**Issue**: fromContainer rules still missing after deployment

**Solution**:
```bash
# 1. Verify you deployed the correct image
kubectl get daemonset aws-node -n kube-system -o jsonpath='{.spec.template.spec.containers[0].image}'

# 2. Check VPC-CNI logs
kubectl logs -n kube-system -l k8s-app=aws-node --tail=100

# 3. Verify commit in running pod
kubectl exec -n kube-system -l k8s-app=aws-node -c aws-node -- \
  /app/aws-vpc-cni --version
```

---

## Success Criteria

Your deployment is successful when:

- ✅ All aws-node pods running and Ready
- ✅ New pods get fromContainer rules (verified with `ip rule list`)
- ✅ Existing pods maintain fromContainer rules after VPC-CNI restart
- ✅ No pods in CrashLoopBackOff or Error state
- ✅ Network connectivity works for all pods
- ✅ ENI reconciliation events don't break pod networking

---

## Next Steps

### After Successful Deployment

1. **Monitor for 1 week** - Ensure stability
2. **Report to AWS** - Share your fix and findings
3. **Wait for official patch** - AWS may release v1.20.5 or v1.21.0
4. **Plan migration** - When official fix available, migrate to AWS version

### Reporting to AWS

Create GitHub issue: https://github.com/aws/amazon-vpc-cni-k8s/issues

**Title**: VPC-CNI v1.20.4 missing fromContainer rules for RT_TABLE_MAIN pods

**Include**:
- Link to your commit: 9a259f69
- Description of bug and fix
- Evidence from your investigation
- Impact assessment
- Test results

---

## Questions?

If you encounter issues:

1. Check VPC-CNI logs: `kubectl logs -n kube-system -l k8s-app=aws-node`
2. Check pod events: `kubectl describe pod <pod-name>`
3. Verify networking: `ip rule list`, `ip route show`
4. Review this document's Troubleshooting section

**Emergency rollback**: Use Option 1 or 2 from Rollback Procedure section
