# This root's own remote state bucket. Bootstrapped in two steps, since the
# backend can't point at a bucket that doesn't exist yet:
#   1. `tofu apply -target=aws_s3_bucket.cloud_tofu_state -target=aws_s3_bucket_versioning.cloud_tofu_state`
#      while still on local state, so the bucket exists.
#   2. Add the `backend "s3" {}` block to this file (see the commented
#      template below), then `tofu init -migrate-state`.
# The state document itself stays encrypted regardless of backend — the
# `encryption { state {...} }` block in providers.tf operates on the state
# document before/after it's handed to whatever backend stores the bytes,
# independent of where those bytes live.
resource "random_id" "cloud_tofu_state" {
  byte_length = 4
}

resource "aws_s3_bucket" "cloud_tofu_state" {
  bucket        = "cloud-tofu-state-${random_id.cloud_tofu_state.hex}"
  force_destroy = false

  lifecycle {
    prevent_destroy = true
  }
}

resource "aws_s3_bucket_versioning" "cloud_tofu_state" {
  bucket = aws_s3_bucket.cloud_tofu_state.id
  versioning_configuration {
    status = "Enabled"
  }
}

# The backend block itself lives in providers.tf's terraform{} block,
# pointed at this bucket by its literal (now-known) name.

output "cloud_tofu_state_bucket" {
  description = "This root's own remote state bucket"
  value       = aws_s3_bucket.cloud_tofu_state.bucket
}
