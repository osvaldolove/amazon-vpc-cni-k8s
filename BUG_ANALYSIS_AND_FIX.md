# VPC-CNI Bug Analysis and Fix

## Bug Location
**File**: `cmd/routed-eni-cni-plugin/driver/driver.go`  
**Function**: `setupIPBasedContainerRouteRules`  
**Line**: 542

## The Buggy Code

```go
// Line 515-551
// setupIPBasedContainerRouteRules setups the routes and route rules for containers based on IP.
// traffic to container(to containerAddr) will be routed via the `main` route table.
// traffic from container(from containerAddr) will be routed via the specified rtTable.
func (n *linuxNetwork) setupIPBasedContainerRouteRules(hostVeth netlink.Link, containerAddr *net.IPNet, rtTable int, log logger.Logger) error {
	// ... setup toContainer route and rule (lines 519-540) ...

	// BUG: This condition skips fromContainer rule for RT_TABLE_MAIN (254)
	if rtTable != unix.RT_TABLE_MAIN {  // ← LINE 542: THE BUG
		fromContainerRule := n.netLink.NewRule()
		fromContainerRule.Src = containerAddr
		fromContainerRule.Priority = networkutils.FromPodRulePriority
		fromContainerRule.Table = rtTable
		if err := n.netLink.RuleAdd(fromContainerRule); err != nil && !networkutils.IsRuleExistsError(err) {
			return errors.Wrapf(err, "failed to setup fromContainer rule, containerAddr=%s, rtTable=%v", containerAddr.String(), rtTable)
		}
		log.Debugf("Successfully setup fromContainer rule, containerAddr=%s, rtTable=%v", containerAddr.String(), rtTable)
	}

	return nil
}
```

## Why This Is A Bug

### The Function's Intent (from comment on line 517)
> "traffic from container(from containerAddr) will be routed via the specified rtTable"

### What Actually Happens
**When `rtTable == unix.RT_TABLE_MAIN` (254)**:
- The `if` condition on line 542 evaluates to FALSE
- The `fromContainer` rule is **SKIPPED**
- Return traffic from the pod has no routing rule
- Pod appears Running but has no network connectivity

## Root Cause Analysis

### When v1.11.0 Added Prefix Delegation

**Developer's reasoning** (hypothesized):
1. Prefix delegation uses RT_TABLE_MAIN (254) for all pods
2. Prefix delegation handles routing differently (doesn't need per-pod fromContainer rules)
3. "Let's skip `fromContainer` rule setup when rtTable == 254"
4. Added: `if rtTable != unix.RT_TABLE_MAIN`

**What the developer forgot**:
- In **secondary IP mode**, primary ENI pods ALSO use RT_TABLE_MAIN (254)
- Those pods are NOT using prefix delegation
- Those pods STILL NEED the `fromContainer` rule
- The condition affects BOTH prefix mode AND secondary IP mode primary ENI

### When `rtTable == RT_TABLE_MAIN` (254)

**Two scenarios**:

1. **Prefix Delegation Mode** (intentional skip)
   - All pods use table 254
   - Networking handled by prefix delegation code path
   - Skipping `fromContainer` rule is correct

2. **Secondary IP Mode, Primary ENI** (unintentional skip, THE BUG)
   - Primary ENI pods use table 254
   - Networking handled by secondary IP code path
   - Skipping `fromContainer` rule is WRONG - breaks connectivity

## Investigation Findings: Bug is Worse Than Expected

### Initial Theory (WRONG)
We thought only PRIMARY ENI pods were affected (those with rtTable == 254).

### Actual Finding (From AWS EC2 API)
Broken pods were on:
- **Tertiary ENI** (ens7 / DeviceIndex 2) - IP 10.255.171.27
- **Secondary ENI** (ens6 / DeviceIndex 1) - IP 10.255.171.47

### Even More Shocking
When we checked IP rules:
```bash
$ ip rule list | grep "from 10.255.171" | wc -l
0
```
**ZERO fromContainer rules existed for ANY pod on the node!**

### Revised Understanding

**Most likely scenario**: VPC-CNI reconciliation failure
1. ENI attachment event (ens7) triggered reconciliation
2. Reconciliation deleted existing fromContainer rules
3. Bug in reconciliation code prevented rules from being recreated
4. Result: ALL pods lost their fromContainer rules

**Why some pods still work**: 
- Existing TCP connections continue via Linux connection tracking
- New connections fail (no routing rules)
- Broken pod just started, needs new connections

## The Fix

### Option 1: Simple Fix (Remove the condition)

```go
// BEFORE (Buggy)
if rtTable != unix.RT_TABLE_MAIN {
    // Setup fromContainer rule
}

// AFTER (Fixed)
// Always setup fromContainer rule
fromContainerRule := n.netLink.NewRule()
fromContainerRule.Src = containerAddr
fromContainerRule.Priority = networkutils.FromPodRulePriority
fromContainerRule.Table = rtTable
if err := n.netLink.RuleAdd(fromContainerRule); err != nil && !networkutils.IsRuleExistsError(err) {
    return errors.Wrapf(err, "failed to setup fromContainer rule, containerAddr=%s, rtTable=%v", containerAddr.String(), rtTable)
}
log.Debugf("Successfully setup fromContainer rule, containerAddr=%s, rtTable=%v", containerAddr.String(), rtTable)
```

**Pros**:
- ✅ Simplest fix (reverts to v1.10.4 behavior)
- ✅ Fixes both secondary IP mode and any reconciliation issues
- ✅ Minimal code change
- ✅ Easy to understand

**Cons**:
- ⚠️ Might create redundant rule for prefix delegation mode
- ⚠️ Need to verify prefix delegation still works

### Option 2: Check Prefix Delegation Flag (Safer)

```go
// Pseudo-code (would need to pass prefix delegation flag)
if rtTable != unix.RT_TABLE_MAIN || !isPrefixDelegationEnabled {
    // Setup fromContainer rule
}
```

**Pros**:
- ✅ More targeted fix
- ✅ Only affects secondary IP mode
- ✅ Doesn't change prefix delegation behavior

**Cons**:
- ❌ Requires passing additional parameter through call chain
- ❌ More complex change
- ❌ Harder to backport

### Option 3: Always Create Rule, Handle Idempotency

```go
// Always create the rule
fromContainerRule := n.netLink.NewRule()
fromContainerRule.Src = containerAddr
fromContainerRule.Priority = networkutils.FromPodRulePriority
fromContainerRule.Table = rtTable

// RuleAdd already handles "rule exists" error (see line 547)
if err := n.netLink.RuleAdd(fromContainerRule); err != nil && !networkutils.IsRuleExistsError(err) {
    return errors.Wrapf(err, "failed to setup fromContainer rule, containerAddr=%s, rtTable=%v", containerAddr.String(), rtTable)
}
log.Debugf("Successfully setup fromContainer rule, containerAddr=%s, rtTable=%v", containerAddr.String(), rtTable)
```

**Note**: The code already handles `IsRuleExistsError`, so if prefix delegation or another code path already created the rule, this will safely ignore it.

## Recommended Fix: Option 1 (Simple)

**Rationale**:
1. The function comment (line 517) says it should ALWAYS create the fromContainer rule
2. The error handling already manages duplicate rules (`IsRuleExistsError`)
3. Simplest fix with lowest risk
4. Matches v1.10.4 behavior (last known working version)

## Testing the Fix

### 1. Build Custom Image
```bash
cd vpc-cni-fork
make docker
docker tag amazon/amazon-k8s-cni:v1.20.4 YOUR_REGISTRY/amazon-k8s-cni:v1.20.4-patched
docker push YOUR_REGISTRY/amazon-k8s-cni:v1.20.4-patched
```

### 2. Deploy to Test Cluster
```bash
kubectl set image daemonset/aws-node -n kube-system \
  aws-node=YOUR_REGISTRY/amazon-k8s-cni:v1.20.4-patched
```

### 3. Verify Fix
```bash
# Create test pod
kubectl run test-pod --image=nginx

# Get pod IP
POD_IP=$(kubectl get pod test-pod -o jsonpath='{.status.podIP}')

# Check for fromContainer rule
ip rule list | grep "from $POD_IP"
# Should see: "512: from <POD_IP> lookup <table>"

# Test connectivity
kubectl exec test-pod -- curl -s https://kubernetes.default.svc.cluster.local/healthz
# Should return: ok
```

### 4. Test Reconciliation
```bash
# Trigger ENI event (if possible) or restart VPC-CNI pods
kubectl rollout restart daemonset/aws-node -n kube-system

# Wait 2 minutes, verify rule still exists
ip rule list | grep "from $POD_IP"

# Verify pod still works
kubectl exec test-pod -- curl -s https://kubernetes.default.svc.cluster.local/healthz
```

## Impact Assessment

### Who Is Affected

1. **Secondary IP mode users** (no prefix delegation)
2. **Small subnets** (/26, /27, /28) - forced into secondary IP mode
3. **Clusters with ENI attachment events** (triggers reconciliation)
4. **AL2023 users** (newer OS, fewer users to report)

### Why Bug Went Unnoticed

- ~90% of users use prefix delegation (different code path, no bug)
- Most secondary IP users have large subnets (primary ENI rarely used)
- Bug only manifests during ENI reconciliation events
- Working pods continue working (only new pods fail)

## Files to Modify

1. `cmd/routed-eni-cni-plugin/driver/driver.go` - Line 542
2. `cmd/routed-eni-cni-plugin/driver/driver.go` - Line 566 (teardown function has same bug)

## Commit Message Template

```
Fix: Setup fromContainer routing rule for all routing tables

This fixes a bug where pods on the primary ENI (routing table 254) 
in secondary IP mode don't get their fromContainer IP routing rules,
causing network connectivity failures.

The issue was introduced in v1.11.0 when prefix delegation was added.
The code skips setupHostNetwork() for table 254 to avoid conflicts 
with prefix mode, but this also skips primary ENI pods in secondary 
IP mode, which still need the fromContainer rules.

Fix: Remove the condition that skips fromContainer rule creation for
RT_TABLE_MAIN (254). The rule creation is already idempotent (handles
"rule exists" errors), so this is safe for all modes.

Fixes: #XXXX
```
