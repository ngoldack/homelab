# This root's own remote state bucket. Bootstrapped in two steps, since the
# backend can't point at a bucket that doesn't exist yet:
#   1. `tofu apply -target=aws_s3_bucket.home_tofu_state -target=aws_s3_bucket_versioning.home_tofu_state`
#      while still on local state, so the bucket exists.
#   2. Add the `backend "s3" {}` block to this file (see the commented
#      template below), then `tofu init -migrate-state`.
# The state document itself stays encrypted regardless of backend — the
# `encryption { state {...} }` block above operates on the state document
# before/after it's handed to whatever backend stores the bytes, independent
# of where those bytes live.
resource "random_id" "home_tofu_state" {
  byte_length = 4
}

resource "aws_s3_bucket" "home_tofu_state" {
  bucket        = "home-tofu-state-${random_id.home_tofu_state.hex}"
  force_destroy = false

  lifecycle {
    prevent_destroy = true
  }

  # Two abandoned buckets before this one hit repeated transient
  # eventual-consistency failures on this Hetzner endpoint (read-back
  # verification right after create, then 403/404 on the bucket for a few
  # minutes even once creation had genuinely succeeded) — resolved each time
  # by waiting and retrying, never by anything wrong with the config itself.
}

resource "aws_s3_bucket_versioning" "home_tofu_state" {
  bucket = aws_s3_bucket.home_tofu_state.id
  versioning_configuration {
    status = "Enabled"
  }
}

# The backend block itself lives in providers.tf's terraform{} block,
# pointed at this bucket by its literal (now-known) name.

output "home_tofu_state_bucket" {
  description = "This root's own remote state bucket"
  value       = aws_s3_bucket.home_tofu_state.bucket
}
