# ProtonMail DNS records for ngoldack.de.
#
# These six rrsets are live in the Hetzner zone and present in Terraform
# state, but were NOT declared in the configuration — so every plan proposed
# destroying them (the domain's MX, SPF/verification TXT, three DKIM CNAMEs
# and DMARC), which would have broken mail for the domain the moment anyone
# ran an un-targeted apply. Declared here so they prevail and stay managed.
#
# Values mirror the live records exactly; the quoted TXT payloads are part of
# the record value (Hetzner stores them including the quotes), which is why
# they are escaped here. Change them only together with ProtonMail's own
# instructions — DKIM selectors must match what Proton serves.

resource "hcloud_zone_rrset" "protonmail_mx" {
  zone = "ngoldack.de"
  name = "@"
  type = "MX"
  records = [
    { value = "10 mail.protonmail.ch." },
    { value = "20 mailsec.protonmail.ch." },
  ]
}

resource "hcloud_zone_rrset" "protonmail_txt" {
  zone = "ngoldack.de"
  name = "@"
  type = "TXT"
  records = [
    { value = "\"protonmail-verification=051823fbd7f5bfe2b7db5f920b8e9b8097b56eaf\"" },
    { value = "\"v=spf1 include:_spf.protonmail.ch mx ~all\"" },
  ]
}

resource "hcloud_zone_rrset" "protonmail_dmarc" {
  zone = "ngoldack.de"
  name = "_dmarc"
  type = "TXT"
  records = [
    { value = "\"v=DMARC1; p=quarantine\"" },
  ]
}

resource "hcloud_zone_rrset" "protonmail_dkim1" {
  zone = "ngoldack.de"
  name = "protonmail._domainkey"
  type = "CNAME"
  records = [
    { value = "protonmail.domainkey.d6hahn4g75p37635ezu4h7hr4d4ugsh6xugtby3psqq5doe6o5l5q.domains.proton.ch." },
  ]
}

resource "hcloud_zone_rrset" "protonmail_dkim2" {
  zone = "ngoldack.de"
  name = "protonmail2._domainkey"
  type = "CNAME"
  records = [
    { value = "protonmail2.domainkey.d6hahn4g75p37635ezu4h7hr4d4ugsh6xugtby3psqq5doe6o5l5q.domains.proton.ch." },
  ]
}

resource "hcloud_zone_rrset" "protonmail_dkim3" {
  zone = "ngoldack.de"
  name = "protonmail3._domainkey"
  type = "CNAME"
  records = [
    { value = "protonmail3.domainkey.d6hahn4g75p37635ezu4h7hr4d4ugsh6xugtby3psqq5doe6o5l5q.domains.proton.ch." },
  ]
}
