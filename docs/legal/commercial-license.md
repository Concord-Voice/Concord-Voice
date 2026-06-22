# Concord Voice Commercial Licensing — Enterprise & MSP/OEM

**Status:** Coming Soon
**Effective Date:** _[TO BE SET ON PUBLICATION]_

> **Coordination note.** This document is the canonical commercial-licensing
> reference for Concord Voice, maintained in the project repository. The
> public-facing Enterprise & MSP page at
> [concordvoice.com/enterprise-msp](https://concordvoice.com/enterprise-msp)
> (currently labeled "Coming Soon") should mirror this content. If the two
> diverge, the public page is intended for marketing presentation and this
> document captures the program structure for internal reference and legal
> coordination with the [LICENSE](../../LICENSE) and
> [Terms of Service](terms-of-service.md).

## Overview

[Concord Voice](https://concordvoice.chat) is source-available software
governed by the [Concord Voice Source License 1.0](../../LICENSE) (the
"License" or "CVSL"). The License grants free use of the Concord Voice
source code for personal and non-profit purposes. Use of the source code
by for-profit and governmental entities — including self-hosted
deployments — requires a commercial license.

Concord Voice's commercial program is structured around two distinct
tracks, designed for two different kinds of organization:

- **Enterprise** — for organizations deploying Concord Voice for their
  own use, with full control over their communications environment.
- **MSP & OEM** — for managed service providers, hosting companies, and
  technology partners offering Concord Voice as part of their own
  product or service portfolio.

Hosted use of the Concord Voice Service at concordvoice.chat is governed
by the [Terms of Service](terms-of-service.md), not by the CVSL.

## When Do You Need a Commercial License?

| Your situation | Path |
|---|---|
| You are an individual using Concord Voice for personal, non-commercial purposes (homelab, family, hobby) | **No commercial license needed.** Personal Use is permitted under Section 2 of the CVSL. |
| You are a non-profit organization (recognized under IRC § 501(c)(3) or foreign equivalent) using Concord Voice for purposes consistent with your non-profit mission | **No commercial license needed.** Non-Profit Use is permitted under Section 2 of the CVSL. |
| You are an educational institution using Concord Voice for educational purposes | **No commercial license needed.** Educational use qualifies as Non-Profit Use. |
| You are a for-profit business, public agency, or governmental entity deploying Concord Voice on your own infrastructure for internal use | **Enterprise track.** See below. |
| You are a managed service provider, hosting company, or technology partner offering Concord Voice (with your branding or ours) to your own customers | **MSP & OEM track.** See below. |
| You are using the hosted concordvoice.chat Service (not self-hosting) | **No commercial license needed.** Your access is governed by the [Terms of Service](terms-of-service.md). |
| You want to build a product or service that competes with Concord Voice | **Not permitted.** Section 4 of the CVSL prohibits Competing Use regardless of commercial license status, unless expressly authorized in writing. |

If you are unsure which track applies, contact us at
[contact-us@concordvoice.com](mailto:contact-us@concordvoice.com).

## Enterprise

**Your Organization. Your Infrastructure. Your Data.**

For companies, agencies, and institutions that need full control over
their communications environment. Deploy Concord Voice on your own
servers, integrate with your existing identity systems, and meet
compliance requirements without trusting a third party with your data.

### Who this is for

- Organizations with data residency or sovereignty requirements
- Teams that need to operate on air-gapped or restricted networks
- Industries with regulatory obligations — legal, healthcare, finance,
  government
- Any organization that won't hand sensitive communications to an
  external platform

### What you get

- Self-hosted deployment on your infrastructure
- End-to-end encrypted voice, video, and messaging
- Full source code access for security audit
- Optional Concord Public Directory listing
- Dedicated support and onboarding directly from Concord Voice
- **Coming Soon:** LDAP/SAML SSO integration
- **Coming Soon:** Audit logging and compliance tooling

### Federal-readiness

Concord Voice's Andromeda initiative has delivered DISA STIG hardening
and FIPS 140-3 cryptographic module compliance ([epic #692](https://github.com/Concord-Voice/Concord-Voice-Alpha/issues/692),
closed). Additional federal-ready capabilities — including post-quantum
cryptography hybrid handshake support — are under consideration for the
v1.1.0 release.

Public-sector customers may require additional security attestations
(FedRAMP, StateRAMP, etc.). Sole-source justification and
authority-to-operate documentation is available on request.
Procurement-vehicle support (GSA Schedule, state cooperative purchasing
vehicles) is available for qualifying engagements.

### Pricing

Enterprise pricing is structured as a **single server license** (for
organizations deploying one Concord Voice server) or a **custom
multi-server agreement** for larger deployments below the MSP Fleet
threshold. Enterprise deployments at or above 10 servers may be more
economically structured as an MSP & OEM Fleet 10 license — contact us
for guidance on which is right for you.

For pricing details and a quote, contact
[contact-us@concordvoice.com](mailto:contact-us@concordvoice.com) with
"Enterprise" in the subject line.

## MSP & OEM

**Host It. Brand It. Sell It.**

For managed service providers, hosting companies, and technology
partners who want to offer Concord Voice as part of their own product
or service portfolio. Buy wholesale server licenses, deploy on your
infrastructure, and sell to your customers — with your branding or
ours.

### Who this is for

- Managed service providers adding communications to their stack
- Hosting companies offering turnkey community platforms
- Technology partners bundling Concord Voice into a larger product
- Resellers and consultancies serving clients with specific
  communications needs

### What you get

- **Wholesale server licensing** starting at 10 servers (Fleet 10)
- **30–40% off retail** self-hosted pricing
- **Resale rights** included in the OEM agreement — your customers,
  your pricing, your relationship
- **Optional white-label branding** via the Livery add-on
- **Tiered support model:**
  - **L1 (your team):** customer-facing first-line support
  - **L2/L3 (Concord Voice):** escalated technical support and
    engineering escalation
- **Optional Public Directory listing** per deployed server
- Dedicated partner onboarding
- **Coming Soon:** LDAP/SAML SSO integration
- **Coming Soon:** Audit logging and compliance tooling

### Fleet Licensing Tiers

| Tier | Servers | Wholesale discount |
|---|---|---|
| **Fleet 10** | 10–24 | _[to be set, ~30%]_ |
| **Fleet 25** | 25–49 | _[to be set]_ |
| **Fleet 50** | 50–99 | _[to be set]_ |
| **Fleet 100+** | 100+ | _[to be set, up to ~40%]_ |

Fleet licenses are negotiated per partner agreement. Reach out to
[contact-us@concordvoice.com](mailto:contact-us@concordvoice.com) with
"MSP & OEM" in the subject line to start a conversation.

### Livery — White-Label Branding Add-On

Livery is the optional white-label branding add-on for MSP & OEM
partners. Under a Livery agreement, partners may:

- Replace Concord Voice branding in the client and server with the
  partner's own brand assets, within the scope authorized by the
  Livery agreement.
- Distribute the customized client to their end customers.
- Co-market the customized product under the partner's brand.

Livery is available as an add-on to any Fleet tier. Use of Livery
without an executed Livery agreement is not permitted under Section 6
of the CVSL (Branding, Trademarks, and Attribution) and is a material
breach of the License.

### Concord Public Directory

The Concord Public Directory is an optional discovery service operated
by Concord Voice that allows self-hosted servers to be listed publicly
for community discovery. Per-server listing is optional and configurable
by the server operator.

- For **Enterprise** deployments: Public Directory listing is available
  at the server-operator's option, useful for community-facing
  deployments and public service offerings.
- For **MSP & OEM** partners: Public Directory listing is configurable
  per-server in the partner's fleet, allowing customer-facing
  deployments to be discoverable while internal or sensitive
  deployments remain private.

Use of the Public Directory is governed by the [Terms of Service](terms-of-service.md);
Public Directory access from non-Official Clients is subject to
Section 4 (Permitted Clients) of the TOS.

## Program Comparison

| Capability | Enterprise | MSP & OEM |
|---|---|---|
| Self-hosted deployment | ✓ | ✓ |
| End-to-end encryption | ✓ | ✓ |
| Source code audit access | ✓ | ✓ |
| Server license (single) | ✓ | — (Fleet licensing instead) |
| Fleet licensing (10+ servers) | — (contact for custom) | ✓ Starting at Fleet 10 |
| Wholesale pricing | — | ✓ 30–40% off retail |
| Resale rights | — | ✓ Included in OEM agreement |
| White-label branding (Livery) | — | ✓ Add-on |
| Concord Public Directory listing | ✓ Optional | ✓ Per-server, optional |
| SSO integration (LDAP/SAML) | ✓ Coming Soon | ✓ Coming Soon |
| Audit logging & compliance | ✓ Coming Soon | ✓ Coming Soon |
| Dedicated onboarding | ✓ | ✓ |
| Support model | Direct from Concord Voice | L1 = partner · L2/L3 = Concord Voice |
| Pricing structure | Single server license or custom | Fleet 10 / 25 / 50 / 100+ tiers |

## Evaluation Period

Section 5 of the CVSL permits a good-faith evaluation period of up to
thirty (30) days for prospective Commercial Licensees to evaluate the
Software for the purpose of determining whether to obtain a Commercial
License.

Evaluation use must be:

- internal only (no distribution to third parties);
- non-production (no use for actual operational, customer-facing, or
  revenue-generating purposes);
- limited to no more than thirty (30) days from first install; and
- not a substitute for a commercial license once production use begins.

If you need additional evaluation time or wish to evaluate in a
production-adjacent environment, contact
[contact-us@concordvoice.com](mailto:contact-us@concordvoice.com) with
"Evaluation Extension" in the subject line.

## Concord Voice Service (Hosted SaaS)

Use of the hosted Concord Voice Service at
[concordvoice.chat](https://concordvoice.chat) is governed by the
[Terms of Service](terms-of-service.md), not by the License. Hosted
Service use does not require a separate commercial license; subscription
fees for the Service are described separately at
[concordvoice.com/pricing](https://concordvoice.com/pricing).

The hosted Service is a distinct offering from Enterprise and MSP/OEM
self-hosted licensing. An organization can simultaneously use the
hosted Service for some users and self-host for others; both paths are
independently licensed.

## Frequently Asked Questions

### Can I use Concord Voice for free?

Yes, if you fall into one of the following categories:

- You are an individual using Concord Voice for personal, non-commercial
  purposes.
- You are a non-profit organization (501(c)(3) or foreign equivalent),
  educational institution, or public-research organization.

If you fall into either category, you may use, modify, and self-host
the Concord Voice source code under the License at no cost.

### Do I need a commercial license for development or testing?

No, provided your development or testing is:

- by an individual for personal purposes; or
- by a non-profit organization for purposes consistent with its
  mission.

For commercial entities, a 30-day evaluation period is permitted for
pre-purchase evaluation; beyond that, a commercial license is required
even for non-production development environments.

### Which track should I choose — Enterprise or MSP & OEM?

The decision rests on whether you are deploying for your **own
organization** (Enterprise) or **on behalf of customers** (MSP & OEM).

- If you are deploying Concord Voice for the internal communications of
  one organization (yours), even if that organization has multiple
  departments, divisions, or related entities, you are an Enterprise
  customer.
- If you are deploying Concord Voice on behalf of distinct third-party
  customers, with each deployment serving a separate customer
  organization, you are an MSP & OEM partner.

The line between "one large organization with many divisions" and
"multiple customer organizations" can be subtle. When in doubt, contact
us — we'd rather help you pick the right track than have to renegotiate
later.

### What is a "server" for the purposes of Fleet licensing?

A "server" for the purposes of Fleet licensing is a single Concord
Voice control-plane deployment serving a logically distinct customer
or community. Multiple servers operated on the same physical or virtual
infrastructure count as separate servers if they serve distinct
customer organizations.

A single server may host multiple Concord Voice communities (analogous
to Discord "servers" within a single Concord Voice instance); this is
not the same as Fleet licensing. Fleet count is at the instance level,
not the community level.

### What counts as a "Competing Use"?

Per Section 4 of the CVSL, Competing Use means any use of the Software
to develop, market, distribute, host, operate, sell, or otherwise make
available a product or service that competes with the Software or with
the hosted Concord Voice Service. This includes any real-time voice,
video, messaging, presence, or collaboration platform offered to third
parties.

The MSP & OEM program is **NOT** a Competing Use — partners reselling
Concord Voice under license are operating within the framework, not
competing with it. The Competing Use prohibition is aimed at parties
attempting to rebuild Concord Voice's core platform under a different
name without paying for the source rights.

If you are unsure whether your intended use is a Competing Use, contact
us before deploying.

### Can MSP & OEM partners use the Livery add-on without disclosing Concord Voice?

Livery permits white-label branding — partners may present the
deployed software under their own brand. However, the source code
attribution requirements of Section 6 of the CVSL still apply: partners
must retain copyright notices in the source code itself. End-user
communications and customer-facing UI may carry the partner's branding;
internal source code retains the Concord Voice attribution.

Specific Livery branding scopes are negotiated per partner agreement.

### Can I fork Concord Voice on GitHub?

Yes. Forking the public source code repository is permitted for
purposes consistent with the License — that is, for Permitted Purposes
(Personal Use, Non-Profit Use) that are not Competing Use.

If you fork to develop a competing platform, your fork is in violation
of Section 4 of the License regardless of whether you publish or
distribute the fork.

### Does my custom client connect to concordvoice.chat?

No. Per Section 4 of the [Terms of Service](terms-of-service.md),
access to the hosted Concord Voice Service is permitted only through
official Concord Voice client software. Custom clients may not connect
to concordvoice.chat.

Custom clients **are** permitted for self-hosted deployments — if you
self-host Concord Voice under a commercial license (or under personal/
non-profit use rights), you may permit any client of your choosing to
connect to your self-hosted instances. The official-client requirement
applies only to the Concord Voice Service operated by Concord Voice.

### What happens on the Change Date (February 15, 2030)?

On the Change Date applicable to a specific version of the Software,
that version is additionally licensed under the [GNU Affero General
Public License, Version 3.0 or later](https://www.gnu.org/licenses/agpl-3.0.html)
(the Change License). After the Change Date for a specific version,
you may use that version either under the License (with its terms and
restrictions) or under the Change License (with AGPL's network-
copyleft obligations), at your option.

The Change Date applies version-by-version: each version of the
Software has its own Change Date, calculated as the earlier of (a)
February 15, 2030 or (b) the fourth anniversary of that version's
first public distribution under the License.

### What happens if I'm already self-hosting under the previous Business Source License (BSL 1.1)?

Versions of Concord Voice released prior to _[CVSL effective date]_
were released under the Business Source License 1.1 (BSL 1.1). Your
use of those prior versions continues to be governed by the BSL 1.1,
including its more permissive Additional Use Grant.

Versions of Concord Voice released on or after _[CVSL effective date]_
are governed by the Concord Voice Source License 1.0 described here.
If you upgrade to a CVSL-licensed version, your use of the upgraded
version is governed by the CVSL.

If you are currently a for-profit self-hoster relying on the BSL 1.1
self-hosting grant, contact
[contact-us@concordvoice.com](mailto:contact-us@concordvoice.com) to
discuss transition options, including grandfathered pricing for early
commercial licensees.

### When will Enterprise and MSP & OEM programs go live?

The programs are currently labeled "Coming Soon" on the public-facing
Enterprise & MSP page. The launch date will coincide with the
CVSL-1.0 effective date and the publication of the new Terms of
Service. Watch
[concordvoice.com/enterprise-msp](https://concordvoice.com/enterprise-msp)
for the live announcement, or contact
[contact-us@concordvoice.com](mailto:contact-us@concordvoice.com) to
register interest and receive early access.

### How does Concord Voice's end-to-end encryption interact with regulated-industry compliance?

E2EE provides strong technical protection for content but does not by
itself constitute regulatory compliance. Concord Voice does not
currently offer a HIPAA Business Associate Agreement, message-retention
features for FINRA/SEC compliance, or full FedRAMP authorization. See
the [Terms of Service](terms-of-service.md) Sections 19, 20, and 21 for
the regulated-industries disclaimer, HIPAA-no-BAA statement, and
export-controls disclosure.

If your use case requires features not currently available, contact us
— some are roadmapped under the Enterprise track ("Coming Soon: SSO,
Audit logging and compliance") and others are candidates for the v1.1.0
release.

### How do I report a license violation?

If you believe someone is using Concord Voice in violation of the
License, contact us at
[contact-us@concordvoice.com](mailto:contact-us@concordvoice.com) with
details. We investigate reports in good faith but do not provide
updates on investigations or actions taken.

### Can I contribute to Concord Voice?

Yes. Contributions are welcome. By submitting a contribution (pull
request, issue, or similar), you agree that your contribution is
licensed under the License and may be relicensed under the Change
License on the applicable Change Date.

For contributions of substantial scope, Concord Voice may request that
you sign a Contributor License Agreement ("CLA"). The CLA confirms
that you have the right to make the contribution and grants Concord
Voice the necessary rights to incorporate it into the Software.

## Contact

For commercial-licensing inquiries (all tracks):
[contact-us@concordvoice.com](mailto:contact-us@concordvoice.com)

Subject-line conventions to route your inquiry quickly:

- `Enterprise:` for Enterprise licensing inquiries
- `MSP & OEM:` for partner program applications
- `Fleet 10` / `Fleet 25` / `Fleet 50` / `Fleet 100+` for specific
  Fleet tier inquiries
- `Livery:` for white-label branding inquiries
- `Evaluation Extension:` for evaluation-period extension requests
- `Government:` for public-sector procurement
- `BSL Transition:` for existing BSL 1.1 self-hosters

For all other inquiries: see the
[Concord Voice contact page](https://concordvoice.com/contact).
