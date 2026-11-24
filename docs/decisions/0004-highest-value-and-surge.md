# 0004: Count the worst case: highest value, autoscaler maximum, rollout surge

**Decision:** when a setting has several values in the source, use the highest. Count pods at the HPA or KEDA maximum, and report the rolling update peak (pods plus maxSurge) separately, as TC102, when only the peak doesn't fit.

**Why:** "too many clients" happens at the worst moment: under load, when the autoscaler is at its maximum, or during a deploy, when old and new pods both hold full pools. A budget that only fits the average doesn't help. TC102 is a warning rather than an error because pools in a rollout's new pods often start empty and fill later.
