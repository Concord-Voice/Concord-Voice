# Terms of Service

**Last Updated:** _[TO BE SET ON PUBLICATION]_

> **Source-of-truth note.** This document is the canonical Terms of Service
> for Concord Voice, maintained in the project repository for review and
> version control. The published Termly.io version at
> [www.concordvoice.com/terms](https://www.concordvoice.com/terms) should
> mirror this document. If the two diverge, this document represents the
> intended terms and the Termly version should be updated to match.

---

## Agreement to Our Legal Terms

We are **Concord Voice LLC** ("Company," "we," "us," "our"), a company
registered in Virginia, United States, at 2008 Bremo Road, Suite 110,
Richmond, VA 23226.

We operate the website [https://www.concordvoice.com](https://www.concordvoice.com)
(the "Site") and any other related products and services that refer or link
to these legal terms (the "Legal Terms") (collectively, the "Services").

Concord Voice is a source-available communications platform that focuses on
the privacy and security of its users through the use of end-to-end
encryption and a low-telemetry architecture.

You can contact us by email at
[contact-us@concordvoice.com](mailto:contact-us@concordvoice.com) or by mail
to 2008 Bremo Road, Suite 110, Richmond, VA 23226, United States.

These Legal Terms constitute a legally binding agreement made between you,
whether personally or on behalf of an entity ("you"), and Concord Voice LLC,
concerning your access to and use of the Services. You agree that by
accessing the Services, you have read, understood, and agreed to be bound by
all of these Legal Terms. **IF YOU DO NOT AGREE WITH ALL OF THESE LEGAL
TERMS, THEN YOU ARE EXPRESSLY PROHIBITED FROM USING THE SERVICES AND YOU
MUST DISCONTINUE USE IMMEDIATELY.**

We will provide you with prior notice of any scheduled changes to the
Services you are using. The modified Legal Terms will become effective upon
posting or notifying you by
[noreply@concordvoice.com](mailto:noreply@concordvoice.com), as stated in
the email message. By continuing to use the Services after the effective
date of any changes, you agree to be bound by the modified terms.

We recommend that you print a copy of these Legal Terms for your records.

---

## Table of Contents

1. [Eligibility and Minimum Age](#1-eligibility-and-minimum-age)
2. [Our Services](#2-our-services)
3. [End-to-End Encryption Disclosure](#3-end-to-end-encryption-disclosure)
4. [Permitted Clients](#4-permitted-clients)
5. [User Accounts and Registration](#5-user-accounts-and-registration)
6. [Acceptable Use and Prohibited Activities](#6-acceptable-use-and-prohibited-activities)
7. [Subscriptions, Payment, and Free Trial](#7-subscriptions-payment-and-free-trial)
8. [User Contributions and Intellectual Property](#8-user-contributions-and-intellectual-property)
9. [Source Code License and Self-Hosting](#9-source-code-license-and-self-hosting)
10. [Software License (Hosted Service Components)](#10-software-license-hosted-service-components)
11. [Mobile Application License](#11-mobile-application-license)
12. [Communications: Transactional and SMS](#12-communications-transactional-and-sms)
13. [Privacy Policy](#13-privacy-policy)
14. [Cross-Border Data Transfers](#14-cross-border-data-transfers)
15. [GDPR and UK Data Protection](#15-gdpr-and-uk-data-protection)
16. [California Users and Residents (CCPA/CPRA)](#16-california-users-and-residents-ccpacpra)
17. [DMCA Notice and Policy](#17-dmca-notice-and-policy)
18. [Law Enforcement and Legal Process](#18-law-enforcement-and-legal-process)
19. [Regulated Industries Disclaimer](#19-regulated-industries-disclaimer)
20. [HIPAA — No Business Associate Agreement](#20-hipaa--no-business-associate-agreement)
21. [Export Controls and Sanctions](#21-export-controls-and-sanctions)
22. [Third-Party Websites and Content](#22-third-party-websites-and-content)
23. [Services Management](#23-services-management)
24. [Term and Termination](#24-term-and-termination)
25. [Modifications and Interruptions](#25-modifications-and-interruptions)
26. [Disclaimer of Warranties](#26-disclaimer-of-warranties)
27. [Limitation of Liability](#27-limitation-of-liability)
28. [Indemnification](#28-indemnification)
29. [User Data](#29-user-data)
30. [Electronic Communications, Transactions, and Signatures](#30-electronic-communications-transactions-and-signatures)
31. [Corrections](#31-corrections)
32. [Governing Law and Dispute Resolution](#32-governing-law-and-dispute-resolution)
33. [Miscellaneous](#33-miscellaneous)
34. [Contact Us](#34-contact-us)

---

## 1. Eligibility and Minimum Age

**1.1** You must be at least 16 years of age to create an account or use the
Concord Voice platform. If you are located in the European Economic Area,
the United Kingdom, or any jurisdiction that sets a higher minimum age for
digital services, you must be at least 16 years of age or the applicable
minimum age in your jurisdiction, whichever is greater.

**1.2** By creating an account, you represent and warrant that you meet the
applicable age requirement. If we become aware that an account belongs to
a user who does not meet the minimum age requirement, we will terminate
that account without notice.

**1.3** If you are between the minimum age and the age of majority in your
jurisdiction, you represent that a parent or legal guardian has reviewed
and agreed to these Terms on your behalf.

---

## 2. Our Services

The information provided when using the Services is not intended for
distribution to or use by any person or entity in any jurisdiction or
country where such distribution or use would be contrary to law or
regulation or which would subject us to any registration requirement
within such jurisdiction or country. Accordingly, those persons who choose
to access the Services from other locations do so on their own initiative
and are solely responsible for compliance with local laws, if and to the
extent local laws are applicable.

---

## 3. End-to-End Encryption Disclosure

**3.1** Concord Voice employs end-to-end encryption ("E2EE") for
user-to-user communications, including messages transmitted in
channels and direct messages, and voice and video streams in
server-channel voice rooms and direct-message 1:1 calls. Messages are
encrypted on your device before transmission using AES-256-GCM, with
encryption keys wrapped using RSA-OAEP 4096-bit encryption. Voice and
video media frames are encrypted on your device using AES-128-GCM via
the WebRTC Encoded Transform API, with frame keys derived from the
same per-channel symmetric keys (via HKDF-SHA256) that protect text
messages; the media server (a mediasoup Selective Forwarding Unit)
forwards the encrypted RTP payloads without seeing plaintext audio or
video content.

**3.2** Concord Voice does not hold, store, or have access to the plaintext
content of end-to-end encrypted communications. We are technically unable
to read, scan, analyze, moderate, or disclose the content of encrypted
messages — whether for internal purposes, advertising, machine learning,
or in response to third-party requests — because we do not possess the
decryption keys.

**3.3** Unencrypted metadata necessary for service operation — including
but not limited to account registration information, timestamps, IP
addresses, server membership, channel membership, and presence status —
may be collected and processed as described in our Privacy Policy.

**3.4** End-to-end encryption protects message content in transit and at
rest on our servers. It does not prevent a recipient from copying,
screenshotting, or otherwise sharing decrypted content on their own
device. Concord Voice is not responsible for actions taken by other users
with content you share.

---

## 4. Permitted Clients

**4.1** Access to the hosted Concord Voice service at example.com
(the "Hosted Service") is permitted only through official Concord Voice
client software distributed by Concord Voice ("Official Clients").
Official Clients are limited to:

- the Concord Voice desktop application for macOS, Windows, and Linux,
  obtained from concordvoice.com, our official software-release channels,
  or operating-system-specific application stores designated by us;
- if and when released, the Concord Voice mobile application for iOS and
  Android obtained from the Apple App Store and Google Play Store,
  respectively; and
- any other client software that we expressly designate in writing as an
  Official Client.

**4.2** You may not access, attempt to access, or facilitate access to the
Hosted Service using any custom, modified, third-party, automated, or
unofficial client software, regardless of the source of such software.
Any access to the Hosted Service using a non-Official Client may, at our
sole discretion, result in immediate suspension or termination of your
account.

**4.3** This Section 4 governs only the Hosted Service operated by Concord
Voice at example.com. **It does not apply to self-hosted deployments
of the Concord Voice source code under the Concord Voice Source License
1.0.** Operators of self-hosted Concord Voice instances may permit any
client of their choosing to connect to their own instances; the Official
Client restriction applies solely to the Hosted Service operated by
Concord Voice.

**4.4** The restriction on non-Official Clients is enforced
contractually through these Terms and may be enforced technically
through cryptographic client attestation or other mechanisms at the
Hosted Service boundary. Attempts to bypass any technical enforcement
measures are a material breach of these Terms.

---

## 5. User Accounts and Registration

You may be required to register to use the Services. You agree to:

- provide accurate, current, and complete information when creating your
  account;
- maintain and promptly update your account information;
- maintain the security of your account credentials, including without
  limitation your password and any multi-factor authentication factor;
- not share your account credentials with any other person; and
- promptly notify us at
  [security@concordvoice.com](mailto:security@concordvoice.com) of any
  unauthorized use of your account.

You are responsible for all activity that occurs under your account.

We reserve the right to remove, reclaim, or change a username you select
if we determine, in our sole discretion, that such username is
inappropriate, obscene, or otherwise objectionable.

By using the Services, you represent and warrant that:

1. all registration information you submit will be true, accurate, current,
   and complete;
2. you will maintain the accuracy of such information and promptly update
   such registration information as necessary;
3. you have the legal capacity to enter into and comply with these Legal
   Terms;
4. you are not under the age of 16, or if a minor, you have received
   parental permission to use the Services;
5. you will not access the Services through automated or non-human means,
   whether through a bot, script, or otherwise;
6. you will not use the Services for any illegal or unauthorized purpose;
   and
7. your use of the Services will not violate any applicable law or
   regulation.

If you provide any information that is untrue, inaccurate, not current, or
incomplete, we have the right to suspend or terminate your account and
refuse any and all current or future use of the Services.

---

## 6. Acceptable Use and Prohibited Activities

You may not access or use the Services for any purpose other than that for
which we make the Services available. As a user of the Services, you agree
not to:

- systematically retrieve data or other content from the Services to
  create or compile, directly or indirectly, a collection, compilation,
  database, or directory without written permission from us;
- trick, defraud, or mislead us and other users, especially in any
  attempt to learn sensitive account information such as user passwords;
- circumvent, disable, or otherwise interfere with security-related
  features of the Services, including features that prevent or restrict
  the use or copying of any Content or enforce limitations on the use of
  the Services and/or the Content contained therein;
- use any information obtained from the Services in order to harass,
  abuse, or harm another person;
- make improper use of our support services or submit false reports of
  abuse or misconduct;
- use the Services in a manner inconsistent with any applicable laws or
  regulations;
- engage in unauthorized framing of or linking to the Services;
- upload or transmit (or attempt to upload or to transmit) viruses,
  Trojan horses, or other material that interferes with any party's
  uninterrupted use and enjoyment of the Services or modifies, impairs,
  disrupts, alters, or interferes with the use, features, functions,
  operation, or maintenance of the Services;
- engage in any automated use of the system, such as using scripts to
  send messages, or using any data mining, robots, or similar data
  gathering and extraction tools, except as may be expressly permitted
  by an authorized public API;
- delete the copyright or other proprietary rights notice from any
  Content;
- attempt to impersonate another user or person, or use the username of
  another user;
- upload or transmit (or attempt to upload or to transmit) any material
  that acts as a passive or active information collection or transmission
  mechanism, including without limitation, clear graphics interchange
  formats ("gifs"), 1×1 pixels, web bugs, cookies, or other similar
  devices (sometimes referred to as "spyware" or "passive collection
  mechanisms" or "pcms");
- interfere with, disrupt, or create an undue burden on the Services or
  the networks or services connected to the Services;
- harass, annoy, intimidate, or threaten any of our employees or agents
  engaged in providing any portion of the Services to you;
- attempt to bypass any measures of the Services designed to prevent or
  restrict access to the Services, including without limitation the
  Official Client restriction in Section 4;
- except as permitted by applicable law (including without limitation
  17 U.S.C. § 1201(f) for interoperability), decipher, decompile,
  disassemble, or reverse engineer any of the software comprising or in
  any way making up a part of the Services;
- except as may be the result of standard search engine or Internet
  browser usage, use, launch, develop, or distribute any automated
  system, including without limitation, any spider, robot, cheat utility,
  scraper, or offline reader that accesses the Services, or use or launch
  any unauthorized script or other software;
- make any unauthorized use of the Services, including collecting
  usernames and/or email addresses of users by electronic or other means
  for the purpose of sending unsolicited email, or creating user accounts
  by automated means or under false pretenses;
- use the Services to develop, market, distribute, host, operate, sell,
  or otherwise make available a product or service that competes with
  the Services or with any commercial offering of Concord Voice (a
  "Competing Use"), except as expressly authorized in writing by Concord
  Voice; or
- sell or otherwise transfer your profile.

Any use of the Services in violation of the foregoing violates these
Legal Terms and may result in, among other things, termination or
suspension of your rights to use the Services.

---

## 7. Subscriptions, Payment, and Free Trial

### 7.1 Payment Methods

We accept the following form of payment:

- Stripe

You agree to provide current, complete, and accurate purchase and account
information for all purchases made via the Services. You further agree to
promptly update account and payment information, including email address,
payment method, and payment card expiration date, so that we can complete
your transactions and contact you as needed. Sales tax will be added to
the price of purchases as deemed required by us. We may change prices at
any time. All payments shall be in US dollars.

You agree to pay all charges at the prices then in effect for your
purchases and any applicable fees, and you authorize us to charge your
chosen payment provider for any such amounts upon placing your order. We
reserve the right to correct any errors or mistakes in pricing, even if
we have already requested or received payment.

We reserve the right to refuse any order placed through the Services. We
may, in our sole discretion, limit or cancel quantities purchased per
person, per household, or per order.

### 7.2 Billing and Renewal

Your subscription will continue and automatically renew unless cancelled.
You consent to our charging your payment method on a recurring basis
without requiring your prior approval for each recurring charge, until
such time as you cancel the applicable order. The length of your billing
cycle is monthly or annually, depending on the plan you select.

### 7.3 Free Trial

We offer a 30-day free trial to new users who register with the Services.
The account will be charged according to the user's chosen subscription
at the end of the free trial.

### 7.4 Cancellation

All purchases are non-refundable. You can cancel your subscription at any
time by logging into your account. Your cancellation will take effect at
the end of the current paid term. If you have any questions or are
unsatisfied with our Services, please email us at
[contact-us@concordvoice.com](mailto:contact-us@concordvoice.com).

### 7.5 Fee Changes

We may, from time to time, make changes to the subscription fee and will
communicate any price changes to you in accordance with applicable law.

---

## 8. User Contributions and Intellectual Property

### 8.1 Our Intellectual Property

We are the owner or licensee of all intellectual property rights in the
Hosted Service, including the Services' source code (subject to the
Concord Voice Source License 1.0, see Section 9), databases,
functionality, software, website designs, audio, video, text,
photographs, and graphics in the Services (collectively, the "Content"),
as well as the trademarks, service marks, and logos contained therein
(the "Marks").

Our Content and Marks are protected by copyright and trademark laws (and
various other intellectual property rights and unfair competition laws)
and treaties in the United States and around the world.

Subject to your compliance with these Legal Terms, including the
Prohibited Activities section above, we grant you a non-exclusive,
non-transferable, revocable license to access the Services and to view
the Content on the Site or in the Official Client. This license is for
your personal use of the Services consistent with these Terms.

Use of the Concord Voice source code is separately governed by the
Concord Voice Source License 1.0 (see Section 9) and is not subject to
this Section 8.

### 8.2 User Contributions

The Services may invite you to chat, contribute to, or participate in
servers, channels, direct messages, and other interactive functionality
in which you may submit, post, display, transmit, perform, publish,
distribute, or broadcast content and materials to other users or through
the Services, including but not limited to text, writings, video, audio,
photographs, graphics, and other material (collectively,
"Contributions").

When you submit Contributions, you represent and warrant that:

- you are the creator and owner of, or have all necessary licenses,
  rights, consents, releases, and permissions to use and to authorize
  the recipients of your Contributions to use, your Contributions in
  the manner contemplated by the Services;
- your Contributions do not infringe the intellectual property rights of
  any third party;
- your Contributions are not false, inaccurate, or misleading;
- your Contributions do not violate any applicable law, regulation, or
  rule, and do not violate the privacy or publicity rights of any third
  party; and
- you have the written consent, release, and/or permission of each and
  every identifiable individual person in your Contributions to use the
  name or likeness of such person.

### 8.3 License to Concord Voice for Service Operation

Because Concord Voice operates an end-to-end encrypted communications
platform, our access to and use of your Contributions is technically
limited as described in Section 3. To the extent we receive any
Contributions in unencrypted form, or any metadata associated with
Contributions, you grant Concord Voice a worldwide, non-exclusive,
royalty-free license to use, store, transmit, display, and process such
Contributions and metadata solely to the extent necessary to operate and
provide the Services to you and other users.

This license is limited to operation of the Services. It does NOT
include:

- use of your Contributions for advertising or marketing;
- use of your Contributions for training of machine-learning or
  artificial-intelligence models;
- sale, resale, sublicensing, or transfer of your Contributions to third
  parties (other than to operate the Service, such as transmitting your
  Contribution to its intended recipient);
- any use of your Contributions inconsistent with the end-to-end
  encryption properties described in Section 3.

You retain all ownership and intellectual property rights in your
Contributions.

### 8.4 Feedback

If you send us suggestions, ideas, feedback, or proposed enhancements
relating to the Services ("Feedback"), you grant us a non-exclusive,
royalty-free, perpetual, irrevocable, and worldwide license to use,
modify, and incorporate the Feedback into the Services without
attribution or compensation. You retain ownership of any pre-existing
intellectual property embodied in your Feedback. We are not required to
treat Feedback as confidential.

### 8.5 Public Contributions

Contributions you post in spaces accessible to other users (e.g., a
public server or channel) may be viewable by, and copied by, those
users. You acknowledge that such Contributions, once decrypted by their
intended recipients, are within those recipients' control, and Concord
Voice cannot prevent further dissemination by those recipients.

### 8.6 Removal of Contributions

Concord Voice has no obligation to monitor any Contributions. We have
the right, in our sole discretion, to remove or refuse to transmit any
Contribution that violates these Legal Terms, where we are technically
able to do so consistent with the Service's end-to-end encryption
properties.

---

## 9. Source Code License and Self-Hosting

### 9.1 Hosted Service vs. Source Code License

These Terms of Service govern your use of the Concord Voice **Hosted
Service** — the servers, infrastructure, and services operated by
Concord Voice at example.com and associated domains.

The Concord Voice **source code** is separately licensed under the
**Concord Voice Source License 1.0** ("CVSL"), the full text of which
is available at
[https://github.com/Concord-Voice/Concord-Voice-Alpha/blob/main/LICENSE](https://github.com/Concord-Voice/Concord-Voice-Alpha/blob/main/LICENSE)
and reproduced in the Concord Voice repository. The CVSL governs your
right to download, modify, self-host, and distribute the Concord Voice
software. These Terms of Service do not modify, supersede, or replace
the CVSL.

### 9.2 Hosted-Only Users

If you use only the Concord Voice Hosted Service and do not download or
deploy the Concord Voice source code, the CVSL does not apply to you.
Your rights and obligations are governed entirely by these Terms of
Service and our Privacy Policy.

### 9.3 Self-Hosted Users

If you self-host Concord Voice on your own infrastructure, your use of
the software is governed by the CVSL. These Terms of Service apply only
if and to the extent that:

- your self-hosted server connects to Concord Voice's hosted
  infrastructure (including but not limited to the Concord Voice
  network, Public Directory, or authentication services); or
- you separately interact with the Hosted Service (for example, by
  creating an account at example.com).

### 9.4 Commercial License for For-Profit and Government Self-Hosting

The CVSL grants free self-hosting rights only for personal use and
non-profit use. For-profit entities and governmental entities require a
commercial license to self-host the Concord Voice source code.
Information regarding commercial licensing is available at
[https://concordvoice.com/commercial-license](https://concordvoice.com/commercial-license)
and at [docs/legal/commercial-license.md](commercial-license.md) in the
repository.

---

## 10. Software License (Hosted Service Components)

If we include software for use in connection with the Hosted Service
(separate from the CVSL-licensed source code), and such software is
accompanied by an end-user license agreement ("EULA"), the terms of the
EULA will govern your use of the software. If such software is not
accompanied by a EULA, then we grant you a non-exclusive, revocable,
personal, and non-transferable license to use such software solely in
connection with the Hosted Service and in accordance with these Legal
Terms.

Any software and any related documentation provided in this manner is
provided "AS IS" without warranty of any kind, either express or implied,
including without limitation the implied warranties of merchantability,
fitness for a particular purpose, or non-infringement. You accept any and
all risk arising out of use or performance of any such software.

This Section 10 does not modify, restrict, or expand the CVSL governing
the Concord Voice source code.

---

## 11. Mobile Application License

If and when Concord Voice releases a mobile application for iOS or
Android (the "App"), and you access the Services via the App, we grant
you a revocable, non-exclusive, non-transferable, limited right to
install and use the App on wireless electronic devices owned or
controlled by you, and to access and use the App on such devices strictly
in accordance with the terms and conditions of this mobile application
license. You shall not:

1. except as permitted by applicable law (including 17 U.S.C. § 1201(f)),
   decompile, reverse engineer, disassemble, attempt to derive the source
   code of, or decrypt the App;
2. make any modification, adaptation, improvement, enhancement,
   translation, or derivative work from the App (other than as expressly
   permitted by the CVSL for source-code modifications);
3. violate any applicable laws, rules, or regulations in connection with
   your access or use of the App;
4. remove, alter, or obscure any proprietary notice (including any notice
   of copyright or trademark) posted by us or the licensors of the App;
5. use the App for any revenue-generating endeavor, commercial
   enterprise, or other purpose for which it is not designed or intended,
   except as authorized under a commercial license under the CVSL;
6. make the App available over a network or other environment permitting
   access or use by multiple devices or users at the same time, except as
   inherent in normal operation of the App;
7. use the App for creating a product, service, or software that is,
   directly or indirectly, competitive with or in any way a substitute
   for the App (a Competing Use under Section 6);
8. use the App to send automated queries to any website or to send any
   unsolicited commercial email; or
9. use any proprietary information or any of our interfaces or our other
   intellectual property in the design, development, manufacture,
   licensing, or distribution of any applications, accessories, or
   devices for use with the App.

### Apple and Android Devices

The following terms apply when you obtain the App from either the Apple
App Store or Google Play Store (each an "App Distributor"):

1. the license granted to you for the App is limited to a non-transferable
   license to use the application on a device that utilizes the Apple iOS
   or Android operating systems, as applicable, and in accordance with
   the usage rules set forth in the applicable App Distributor's terms of
   service;
2. we are responsible for providing any maintenance and support services
   with respect to the App as specified in the terms and conditions of
   this mobile application license or as otherwise required under
   applicable law, and you acknowledge that each App Distributor has no
   obligation whatsoever to furnish any maintenance and support services
   with respect to the App;
3. in the event of any failure of the App to conform to any applicable
   warranty, you may notify the applicable App Distributor, and the App
   Distributor, in accordance with its terms and policies, may refund the
   purchase price, if any, paid for the App, and to the maximum extent
   permitted by applicable law, the App Distributor will have no other
   warranty obligation whatsoever with respect to the App;
4. you represent and warrant that (i) you are not located in a country
   that is subject to a US government embargo, or that has been
   designated by the US government as a "terrorist supporting" country,
   and (ii) you are not listed on any US government list of prohibited
   or restricted parties;
5. you must comply with applicable third-party terms of agreement when
   using the App; and
6. you acknowledge and agree that the App Distributors are third-party
   beneficiaries of the terms and conditions in this mobile application
   license, and that each App Distributor will have the right (and will
   be deemed to have accepted the right) to enforce the terms and
   conditions in this mobile application license against you as a
   third-party beneficiary thereof.

---

## 12. Communications: Transactional and SMS

### 12.1 Transactional Communications

By creating an account, you consent to receive transactional
communications from Concord Voice, including but not limited to: account
verification emails, password reset notifications, security alerts,
payment receipts, and service-critical announcements.

Transactional communications are necessary for the operation and
security of your account and cannot be opted out of while your account
remains active.

### 12.2 Marketing Communications

Marketing communications, product announcements, newsletters, and other
non-essential communications are strictly opt-in. You will not receive
marketing communications unless you have explicitly requested them, and
you may unsubscribe at any time.

Concord Voice will never sell, rent, or share your email address or
contact information with third parties for their marketing purposes.

### 12.3 SMS Text Messaging

**Program description.** By opting in to receive text messages, you
expressly consent to receive text messages (SMS) to your mobile number.
SMS use within Concord Voice is currently limited to multi-factor
authentication codes. When you opt in to receiving text messages, it is
associated with multi-factor authentication. You will receive as many
text messages as there are authentication requests for your account in
which SMS is explicitly selected as the method of authentication.

**Opting out.** If at any time you wish to stop receiving text messages
from us, go to your user account and disable features associated with
SMS, including multi-factor authentication via SMS. We strongly recommend
configuring an alternative multi-factor authentication method (such as a
hardware security key, authenticator app, or passkey) before disabling
SMS multi-factor authentication.

**Message and data rates.** Please be aware that message and data rates
may apply to any SMS messages sent or received. The rates are determined
by your carrier and the specifics of your mobile plan. Carriers are not
liable for delayed or undelivered messages. If you have any questions
about your text plan or data plan, contact your wireless provider.

**Support.** If you have any questions or need assistance regarding our
SMS communications, you can email us at
[contact-us@concordvoice.com](mailto:contact-us@concordvoice.com). If you
have any questions regarding privacy, please read our Privacy Policy at
[http://www.concordvoice.com/privacy-policy](http://www.concordvoice.com/privacy-policy).

---

## 13. Privacy Policy

We care about data privacy and security. Please review our Privacy Policy
at
[http://www.concordvoice.com/privacy-policy](http://www.concordvoice.com/privacy-policy).
By using the Services, you agree to be bound by our Privacy Policy, which
is incorporated into these Legal Terms.

The Services are hosted in the United States. If you access the Services
from any other region of the world with laws or other requirements
governing personal data collection, use, or disclosure that differ from
applicable laws in the United States, then through your continued use of
the Services, you are transferring your data to the United States, and
you expressly consent to have your data transferred to and processed in
the United States, subject to the safeguards described in Section 14
and Section 15.

We do not knowingly accept, request, or solicit information from children
under 13 or knowingly market to children under 13. In accordance with the
U.S. Children's Online Privacy Protection Act, if we receive actual
knowledge that anyone under the age of 13 has provided personal
information to us without verifiable parental consent, we will delete
that information from the Services as quickly as is reasonably practical.

---

## 14. Cross-Border Data Transfers

**14.1** Concord Voice operates infrastructure that may be located in
multiple jurisdictions. By using the Services, you acknowledge that your
data may be processed and stored in countries other than your country of
residence.

**14.2** Concord Voice will not transfer personal data to any jurisdiction
that does not provide an adequate level of data protection without
implementing appropriate safeguards, including Standard Contractual
Clauses approved by the European Commission or equivalent mechanisms
recognized under applicable law.

**14.3** **Self-hosted servers.** Self-hosted Concord Voice servers are
deployed and operated by the user or organization that maintains them.
Concord Voice has no access to, control over, or responsibility for data
stored on self-hosted infrastructure. The operator of a self-hosted
server is the data controller for all data processed on that server and
is solely responsible for compliance with applicable data protection
laws.

---

## 15. GDPR and UK Data Protection

**15.1 Data Controller.** For users in the European Economic Area (EEA)
and the United Kingdom, Concord Voice acts as the data controller for
personal data processed in connection with your use of the Hosted
Service, as defined under the General Data Protection Regulation (EU
2016/679) and the UK Data Protection Act 2018.

**15.2 Legal Basis for Processing.** Concord processes personal data
under the following legal bases:

- **Contractual necessity** — processing required to provide the Concord
  Voice service, including account creation, authentication, message
  delivery, and voice/video connectivity;
- **Legitimate interest** — processing necessary for platform security,
  abuse prevention, rate limiting, and service improvement, where such
  interests are not overridden by your rights and freedoms;
- **Consent** — processing for optional features, marketing
  communications, and any data collection not covered by contractual
  necessity or legitimate interest. Consent may be withdrawn at any
  time.

**15.3 Your Rights.** Under applicable data protection law, you have the
right to:

- **Access** — request a copy of the personal data Concord Voice holds
  about you;
- **Rectification** — request correction of inaccurate personal data;
- **Erasure** — request deletion of your personal data ("right to be
  forgotten");
- **Restriction** — request that Concord Voice restrict processing of
  your personal data in certain circumstances;
- **Portability** — request a machine-readable copy of your personal
  data for transfer to another service;
- **Objection** — object to processing based on legitimate interest;
- **Withdraw consent** — withdraw previously granted consent at any
  time, without affecting the lawfulness of processing prior to
  withdrawal.

**15.4** To exercise any of these rights, contact us at
[privacy@concordvoice.com](mailto:privacy@concordvoice.com), or use our
self-serve data-subject-request portal at
[https://app.termly.io/dsar/ca286c6c-69cc-471c-8d5b-0e52ce73b3ce](https://app.termly.io/dsar/ca286c6c-69cc-471c-8d5b-0e52ce73b3ce).
We will respond within 30 days. If we require additional time, we will
notify you of the reason and extension period as permitted by law.

**15.5 Data Retention.** Concord retains personal data only for as long
as necessary to provide the service and fulfill the purposes described
in our Privacy Policy. Account data is deleted within 30 days of account
deletion, except where retention is required by law.

**15.6 Supervisory Authority.** If you believe Concord Voice has
violated your data protection rights, you have the right to lodge a
complaint with your local supervisory authority. A list of EEA
supervisory authorities is available at the European Data Protection
Board website.

---

## 16. California Users and Residents (CCPA/CPRA)

California residents have the rights set forth in the California
Consumer Privacy Act, as amended by the California Privacy Rights Act
("CCPA/CPRA"), including the rights to:

- know what personal information we collect, use, disclose, and sell
  about you;
- delete your personal information;
- opt out of the sale or sharing of your personal information (Concord
  Voice does not sell personal information);
- correct inaccurate personal information; and
- limit the use of your sensitive personal information.

To exercise any of these rights, contact us at
[privacy@concordvoice.com](mailto:privacy@concordvoice.com) or use our
self-serve data-subject-request portal at
[https://app.termly.io/dsar/ca286c6c-69cc-471c-8d5b-0e52ce73b3ce](https://app.termly.io/dsar/ca286c6c-69cc-471c-8d5b-0e52ce73b3ce).
We will respond as required under applicable law.

If any complaint with us is not satisfactorily resolved, you can contact
the Complaint Assistance Unit of the Division of Consumer Services of
the California Department of Consumer Affairs in writing at 1625 North
Market Blvd., Suite N 112, Sacramento, California 95834 or by telephone
at (800) 952-5210 or (916) 445-1254.

---

## 17. DMCA Notice and Policy

### 17.1 Designated Agent

Concord Voice has designated an agent to receive notifications of
claimed copyright infringement in accordance with the Digital Millennium
Copyright Act, 17 U.S.C. § 512:

> General Counsel — Copyright
> Attn: Copyright Agent
> 2008 Bremo Road, Suite 110
> Richmond, VA 23226
> United States
> Email:
> [copyright-claims@concordvoice.com](mailto:copyright-claims@concordvoice.com)

### 17.2 Takedown Requests

If you believe that content hosted on or transmitted through the Concord
Voice platform infringes your copyright, you may submit a written
notification containing:

1. identification of the copyrighted work claimed to have been infringed;
2. identification of the material that is claimed to be infringing and
   information sufficient to permit us to locate the material;
3. your contact information, including name, address, telephone number,
   and email address;
4. a statement that you have a good-faith belief that use of the material
   in the manner complained of is not authorized by the copyright owner,
   its agent, or the law;
5. a statement, under penalty of perjury, that the information in the
   notification is accurate and that you are authorized to act on behalf
   of the copyright owner; and
6. your physical or electronic signature.

### 17.3 Limitations due to End-to-End Encryption

Due to the end-to-end encrypted nature of communications on Concord
Voice, we may be unable to access or review the content of encrypted
messages. Takedown requests related to content that Concord Voice cannot
access or decrypt may not be actionable. In such cases, we will notify
the requesting party of this limitation.

### 17.4 Counter-Notification

If you believe that material you posted was removed in error, you may
submit a counter-notification containing:

1. identification of the material that was removed and its prior
   location;
2. a statement under penalty of perjury that you have a good-faith
   belief the material was removed by mistake or misidentification;
3. your name, address, and telephone number;
4. a statement that you consent to the jurisdiction of the federal
   district court for the judicial district in which your address is
   located (or, if your address is outside the United States, for the
   federal district in which Concord Voice is located) and that you will
   accept service of process from the complaining party; and
5. your physical or electronic signature.

If we receive a valid counter-notification meeting the requirements
above, we will restore the removed or disabled material unless we first
receive notice from the party filing the original notification informing
us that such party has filed a court action to restrain you from
engaging in infringing activity related to the material in question.

### 17.5 Repeat Infringers

Concord Voice reserves the right to terminate the accounts of users who
are determined to be repeat infringers.

---

## 18. Law Enforcement and Legal Process

**18.1** Concord Voice will comply with valid legal process served in
accordance with applicable law, including subpoenas, court orders, and
search warrants issued by courts of competent jurisdiction.

**18.2** Due to the end-to-end encrypted architecture of the platform,
Concord Voice does not possess and cannot provide the plaintext content
of encrypted communications. This is a technical limitation, not a
policy choice.

**18.3** Information that Concord Voice may be able to provide in
response to valid legal process includes, where available and applicable:

- account registration information (email address, username, date of
  account creation);
- IP address logs and access timestamps;
- server and channel membership;
- payment records (if applicable);
- unencrypted metadata as described in Section 3.3.

**18.4** Concord Voice will notify affected users of legal process
requests unless prohibited by law, gag order, or court order from doing
so, or where notification would jeopardize an active investigation as
determined by the requesting authority.

**18.5** Concord Voice does not voluntarily provide user data to law
enforcement or government agencies absent valid legal process, except in
cases involving an imminent threat to life or the physical safety of an
individual, in which case Concord Voice may disclose information
necessary to prevent harm at its discretion.

**18.6** Concord Voice publishes a transparency report annually
disclosing the number and type of legal process requests received,
complied with, and challenged. This report does not include information
that would identify individual users or compromise ongoing legal
proceedings.

---

## 19. Regulated Industries Disclaimer

**19.1** Concord Voice is a general-purpose communications platform. It
is not specifically designed, certified, or warranted for use in
industries subject to specialized regulatory requirements, including but
not limited to healthcare (HIPAA), financial services (SOX, FINRA, SEC,
MiFID II), government (FedRAMP, ITAR), or critical infrastructure.

**19.2** Users and organizations operating in regulated industries are
solely responsible for determining whether their use of Concord Voice
meets their applicable regulatory and compliance obligations, including
but not limited to record retention requirements, audit logging, access
controls, and data handling standards.

**19.3** Concord Voice does not currently offer message archival,
regulatory retention, or compliance-specific reporting features.
Organizations subject to message-retention regulations (including but
not limited to FINRA Rule 3110, SEC Rule 17a-4, and MiFID II) should not
rely on Concord Voice as their system of record for regulated
communications.

**19.4** Nothing in these Terms constitutes legal, regulatory, or
compliance advice. Users are encouraged to consult with qualified legal
counsel regarding their specific obligations before deploying Concord
Voice in a regulated environment.

---

## 20. HIPAA — No Business Associate Agreement

**20.1** Concord Voice does not currently operate as a Business
Associate as defined under the Health Insurance Portability and
Accountability Act of 1996 (HIPAA) and its implementing regulations.

**20.2** No Business Associate Agreement ("BAA") is in effect between
Concord Voice and any user unless a separate written BAA has been
individually negotiated and executed.

**20.3** Users subject to HIPAA — including covered entities and their
business associates — must not use Concord Voice to create, receive,
maintain, or transmit protected health information ("PHI") or electronic
protected health information ("ePHI") absent an executed BAA.

**20.4** Concord Voice's end-to-end encryption architecture means that
Concord Voice cannot access the content of encrypted messages. However,
encryption alone does not constitute HIPAA compliance. A BAA,
administrative safeguards, and access controls are independently
required.

**20.5** Concord Voice reserves the right to offer BAA agreements to
qualifying organizations in the future. Availability will be announced
on our website and will require a separate agreement.

---

## 21. Export Controls and Sanctions

**21.1** Concord Voice incorporates cryptographic functionality and may
be subject to export-control laws and regulations, including but not
limited to the United States Export Administration Regulations ("EAR"),
administered by the Bureau of Industry and Security ("BIS"), and Council
Regulation (EU) 2021/821 on dual-use items.

**21.2** By downloading, accessing, or using Concord Voice, you
represent and warrant that:

1. you are not located in, a national or resident of, or under the
   control of any country or territory subject to comprehensive United
   States, European Union, United Kingdom, or United Nations Security
   Council sanctions, including without limitation Cuba, Iran, North
   Korea, Syria, and the Crimea, Donetsk, Luhansk, Kherson, and
   Zaporizhzhia regions of Ukraine;
2. you are not listed on any government restricted-party list, including
   the Specially Designated Nationals (SDN) list maintained by the
   Office of Foreign Assets Control ("OFAC"), the Entity List maintained
   by BIS, the EU Consolidated List of Sanctions, the UK Office of
   Financial Sanctions Implementation Consolidated List, or the UN
   Security Council Consolidated List;
3. you are not 50% or more owned or controlled by any person or entity
   listed on any of the lists in paragraph (2); and
4. you will not use, export, re-export, or transfer Concord Voice in
   violation of any applicable export-control, sanctions, or
   anti-money-laundering laws.

**21.3** Users are solely responsible for compliance with all applicable
import and export laws in their jurisdiction. Concord Voice makes no
representation that the platform is appropriate or available for use in
every jurisdiction.

---

## 22. Third-Party Websites and Content

**22.1** Concord Voice allows users to share links to external websites,
services, and resources in messages, channels, and direct messages.
These links are user-generated content and do not constitute
endorsement, verification, or affiliation by Concord Voice.

**22.2** Concord Voice does not monitor, review, or verify the content,
privacy practices, security, or availability of any third-party website
linked to by users. You access external links at your own risk.

**22.3** Concord Voice is not responsible for any damage, loss, or
liability arising from your interaction with third-party websites or
services accessed through links shared on the platform.

**22.4** If you believe a user is sharing links to malicious, harmful,
or illegal content, you may report it using Concord Voice's reporting
tools. Concord Voice reserves the right to take action on reported
content in accordance with our Acceptable Use Policy, subject to the
limitations described in Section 3 regarding encrypted content.

**22.5** The Services may also contain (or you may be sent via the Site
or any Concord Voice client) links to other websites and to articles,
photographs, text, graphics, pictures, designs, music, sound, video,
information, applications, software, and other content or items
belonging to or originating from third parties ("Third-Party Websites"
and "Third-Party Content"). Such Third-Party Websites and Third-Party
Content are not investigated, monitored, or checked for accuracy,
appropriateness, or completeness by us, and we are not responsible for
any Third-Party Websites accessed through the Services or any
Third-Party Content posted on, available through, or installed from the
Services. Inclusion of, linking to, or permitting the use or
installation of any Third-Party Websites or any Third-Party Content does
not imply approval or endorsement thereof by us.

---

## 23. Services Management

We reserve the right, but not the obligation, to:

1. monitor the Services for violations of these Legal Terms (subject to
   the technical limitations of end-to-end encryption);
2. take appropriate legal action against anyone who, in our sole
   discretion, violates the law or these Legal Terms, including without
   limitation reporting such user to law enforcement authorities;
3. in our sole discretion and without limitation, refuse, restrict
   access to, limit the availability of, or disable (to the extent
   technologically feasible) any of your Contributions or any portion
   thereof;
4. in our sole discretion and without limitation, notice, or liability,
   to remove from the Services or otherwise disable all files and
   content that are excessive in size or are in any way burdensome to
   our systems; and
5. otherwise manage the Services in a manner designed to protect our
   rights and property and to facilitate the proper functioning of the
   Services.

---

## 24. Term and Termination

These Legal Terms shall remain in full force and effect while you use
the Services. WITHOUT LIMITING ANY OTHER PROVISION OF THESE LEGAL TERMS,
WE RESERVE THE RIGHT TO, IN OUR SOLE DISCRETION AND WITHOUT NOTICE OR
LIABILITY, DENY ACCESS TO AND USE OF THE SERVICES (INCLUDING BLOCKING
CERTAIN IP ADDRESSES) TO ANY PERSON FOR ANY REASON OR FOR NO REASON,
INCLUDING WITHOUT LIMITATION FOR BREACH OF ANY REPRESENTATION, WARRANTY,
OR COVENANT CONTAINED IN THESE LEGAL TERMS OR OF ANY APPLICABLE LAW OR
REGULATION. WE MAY TERMINATE YOUR USE OR PARTICIPATION IN THE SERVICES
OR DELETE YOUR ACCOUNT AND ANY CONTENT OR INFORMATION THAT YOU POSTED AT
ANY TIME, WITHOUT WARNING, IN OUR SOLE DISCRETION.

If we terminate or suspend your account for any reason, you are
prohibited from registering and creating a new account under your name,
a fake or borrowed name, or the name of any third party, even if you may
be acting on behalf of the third party. In addition to terminating or
suspending your account, we reserve the right to take appropriate legal
action, including without limitation pursuing civil, criminal, and
injunctive redress.

---

## 25. Modifications and Interruptions

We reserve the right to change, modify, or remove the contents of the
Services at any time or for any reason at our sole discretion without
notice. However, we have no obligation to update any information on our
Services. We will not be liable to you or any third party for any
modification, price change, suspension, or discontinuance of the
Services.

We cannot guarantee the Services will be available at all times. We may
experience hardware, software, or other problems or need to perform
maintenance related to the Services, resulting in interruptions, delays,
or errors. We reserve the right to change, revise, update, suspend,
discontinue, or otherwise modify the Services at any time or for any
reason without notice to you. You agree that we have no liability
whatsoever for any loss, damage, or inconvenience caused by your
inability to access or use the Services during any downtime or
discontinuance of the Services. Nothing in these Legal Terms will be
construed to obligate us to maintain and support the Services or to
supply any corrections, updates, or releases in connection therewith.

---

## 26. Disclaimer of Warranties

THE SERVICES ARE PROVIDED ON AN AS-IS AND AS-AVAILABLE BASIS. YOU AGREE
THAT YOUR USE OF THE SERVICES WILL BE AT YOUR SOLE RISK. TO THE FULLEST
EXTENT PERMITTED BY LAW, WE DISCLAIM ALL WARRANTIES, EXPRESS OR IMPLIED,
IN CONNECTION WITH THE SERVICES AND YOUR USE THEREOF, INCLUDING, WITHOUT
LIMITATION, THE IMPLIED WARRANTIES OF MERCHANTABILITY, FITNESS FOR A
PARTICULAR PURPOSE, AND NON-INFRINGEMENT. WE MAKE NO WARRANTIES OR
REPRESENTATIONS ABOUT THE ACCURACY OR COMPLETENESS OF THE SERVICES'
CONTENT OR THE CONTENT OF ANY WEBSITES OR MOBILE APPLICATIONS LINKED TO
THE SERVICES AND WE WILL ASSUME NO LIABILITY OR RESPONSIBILITY FOR ANY
(1) ERRORS, MISTAKES, OR INACCURACIES OF CONTENT AND MATERIALS, (2)
PERSONAL INJURY OR PROPERTY DAMAGE, OF ANY NATURE WHATSOEVER, RESULTING
FROM YOUR ACCESS TO AND USE OF THE SERVICES, (3) ANY UNAUTHORIZED ACCESS
TO OR USE OF OUR SECURE SERVERS AND/OR ANY AND ALL PERSONAL INFORMATION
AND/OR FINANCIAL INFORMATION STORED THEREIN, (4) ANY INTERRUPTION OR
CESSATION OF TRANSMISSION TO OR FROM THE SERVICES, (5) ANY BUGS,
VIRUSES, TROJAN HORSES, OR THE LIKE WHICH MAY BE TRANSMITTED TO OR
THROUGH THE SERVICES BY ANY THIRD PARTY, AND/OR (6) ANY ERRORS OR
OMISSIONS IN ANY CONTENT AND MATERIALS OR FOR ANY LOSS OR DAMAGE OF ANY
KIND INCURRED AS A RESULT OF THE USE OF ANY CONTENT POSTED, TRANSMITTED,
OR OTHERWISE MADE AVAILABLE VIA THE SERVICES.

---

## 27. Limitation of Liability

IN NO EVENT WILL WE OR OUR DIRECTORS, EMPLOYEES, OR AGENTS BE LIABLE TO
YOU OR ANY THIRD PARTY FOR ANY DIRECT, INDIRECT, CONSEQUENTIAL,
EXEMPLARY, INCIDENTAL, SPECIAL, OR PUNITIVE DAMAGES, INCLUDING LOST
PROFIT, LOST REVENUE, LOSS OF DATA, OR OTHER DAMAGES ARISING FROM YOUR
USE OF THE SERVICES, EVEN IF WE HAVE BEEN ADVISED OF THE POSSIBILITY OF
SUCH DAMAGES.

NOTWITHSTANDING ANYTHING TO THE CONTRARY CONTAINED HEREIN, OUR LIABILITY
TO YOU FOR ANY CAUSE WHATSOEVER AND REGARDLESS OF THE FORM OF THE
ACTION, WILL AT ALL TIMES BE LIMITED TO THE AMOUNT PAID, IF ANY, BY YOU
TO US DURING THE SIX (6) MONTH PERIOD PRIOR TO ANY CAUSE OF ACTION
ARISING.

CERTAIN US STATE LAWS AND INTERNATIONAL LAWS DO NOT ALLOW LIMITATIONS ON
IMPLIED WARRANTIES OR THE EXCLUSION OR LIMITATION OF CERTAIN DAMAGES. IF
THESE LAWS APPLY TO YOU, SOME OR ALL OF THE ABOVE DISCLAIMERS OR
LIMITATIONS MAY NOT APPLY TO YOU, AND YOU MAY HAVE ADDITIONAL RIGHTS.

---

## 28. Indemnification

You agree to defend, indemnify, and hold us harmless, including our
subsidiaries, affiliates, and all of our respective officers, agents,
partners, and employees, from and against any loss, damage, liability,
claim, or demand, including reasonable attorneys' fees and expenses,
made by any third party due to or arising out of:

1. your Contributions;
2. your use of the Services;
3. your breach of these Legal Terms;
4. any breach of your representations and warranties set forth in these
   Legal Terms;
5. your violation of the rights of a third party, including but not
   limited to intellectual property rights; or
6. any overt harmful act toward any other user of the Services with whom
   you connected via the Services.

Notwithstanding the foregoing, we reserve the right, at your expense, to
assume the exclusive defense and control of any matter for which you are
required to indemnify us, and you agree to cooperate, at your expense,
with our defense of such claims. We will use reasonable efforts to
notify you of any such claim, action, or proceeding that is subject to
this indemnification upon becoming aware of it.

---

## 29. User Data

We will maintain certain data that you transmit to the Services for the
purpose of managing the performance of the Services, as well as data
relating to your use of the Services. **As described in Section 3, we
do not have access to the plaintext content of end-to-end encrypted
communications.** Although we perform regular routine backups of data
within our technical access, you are solely responsible for all data
that you transmit or that relates to any activity you have undertaken
using the Services. You agree that, except as required by applicable
data-protection law, we shall have no liability to you for any loss or
corruption of any such data, and you hereby waive any right of action
against us arising from any such loss or corruption of such data.

---

## 30. Electronic Communications, Transactions, and Signatures

Visiting the Services, sending us emails, and completing online forms
constitute electronic communications. You consent to receive electronic
communications, and you agree that all agreements, notices, disclosures,
and other communications we provide to you electronically — via email
and on the Services — satisfy any legal requirement that such
communication be in writing. YOU HEREBY AGREE TO THE USE OF ELECTRONIC
SIGNATURES, CONTRACTS, ORDERS, AND OTHER RECORDS, AND TO ELECTRONIC
DELIVERY OF NOTICES, POLICIES, AND RECORDS OF TRANSACTIONS INITIATED OR
COMPLETED BY US OR VIA THE SERVICES. You hereby waive any rights or
requirements under any statutes, regulations, rules, ordinances, or
other laws in any jurisdiction which require an original signature or
delivery or retention of non-electronic records, or to payments or the
granting of credits by any means other than electronic means.

---

## 31. Corrections

There may be information on the Services that contains typographical
errors, inaccuracies, or omissions, including descriptions, pricing,
availability, and various other information. We reserve the right to
correct any errors, inaccuracies, or omissions and to change or update
the information on the Services at any time, without prior notice.

---

## 32. Governing Law and Dispute Resolution

### 32.1 Governing Law

These Legal Terms and your use of the Services are governed by and
construed in accordance with the laws of the Commonwealth of Virginia,
applicable to agreements made and to be entirely performed within the
Commonwealth of Virginia, without regard to its conflict-of-laws
principles.

### 32.2 Informal Negotiations

To expedite resolution and control the cost of any dispute, controversy,
or claim related to these Legal Terms (each a "Dispute" and collectively
the "Disputes") brought by either you or us (individually, a "Party" and
collectively the "Parties"), the Parties agree to first attempt to
negotiate any Dispute (except those Disputes expressly excluded below)
informally for at least thirty (30) days before initiating arbitration.
Such informal negotiations commence upon written notice from one Party
to the other Party.

### 32.3 Binding Arbitration

If the Parties are unable to resolve a Dispute through informal
negotiations, the Dispute (except those Disputes expressly excluded
below) will be finally and exclusively resolved by binding arbitration.
**YOU UNDERSTAND THAT WITHOUT THIS PROVISION, YOU WOULD HAVE THE RIGHT
TO SUE IN COURT AND HAVE A JURY TRIAL.** The arbitration shall be
commenced and conducted under the Commercial Arbitration Rules of the
American Arbitration Association ("AAA") and, where appropriate, the
AAA's Supplementary Procedures for Consumer Related Disputes ("AAA
Consumer Rules"), both of which are available at the American
Arbitration Association website at
[http://www.adr.org](http://www.adr.org). Your arbitration fees and your
share of arbitrator compensation shall be governed by the AAA Consumer
Rules and, where appropriate, limited by the AAA Consumer Rules. If
such costs are determined by the arbitrator to be excessive, we will
pay all arbitration fees and expenses. The arbitration may be conducted
in person, through the submission of documents, by phone, or online.
The arbitrator will make a decision in writing, but need not provide a
statement of reasons unless requested by either Party. The arbitrator
must follow applicable law, and any award may be challenged if the
arbitrator fails to do so. Except where otherwise required by the
applicable AAA rules or applicable law, the arbitration will take place
in Fairfax County, Virginia. Except as otherwise provided herein, the
Parties may litigate in court to compel arbitration, stay proceedings
pending arbitration, or to confirm, modify, vacate, or enter judgment on
the award entered by the arbitrator.

### 32.4 Court Litigation

If for any reason a Dispute proceeds in court rather than arbitration,
the Dispute shall be commenced or prosecuted in the state and federal
courts located in Fairfax County, Virginia, and the Parties hereby
consent to, and waive all defenses of lack of personal jurisdiction and
forum non conveniens with respect to venue and jurisdiction in such
state and federal courts. Application of the United Nations Convention
on Contracts for the International Sale of Goods and the Uniform
Computer Information Transaction Act ("UCITA") are excluded from these
Legal Terms.

### 32.5 Statute of Limitations

In no event shall any Dispute brought by either Party related in any
way to the Services be commenced more than two (2) years after the
cause of action arose. If this provision is found to be illegal or
unenforceable, then neither Party will elect to arbitrate any Dispute
falling within that portion of this provision found to be illegal or
unenforceable and such Dispute shall be decided by a court of competent
jurisdiction within the courts listed for jurisdiction above, and the
Parties agree to submit to the personal jurisdiction of that court.

### 32.6 Restrictions

The Parties agree that any arbitration shall be limited to the Dispute
between the Parties individually. To the full extent permitted by law,
(a) no arbitration shall be joined with any other proceeding; (b) there
is no right or authority for any Dispute to be arbitrated on a
class-action basis or to utilize class-action procedures; and (c) there
is no right or authority for any Dispute to be brought in a purported
representative capacity on behalf of the general public or any other
persons.

### 32.7 Exceptions to Informal Negotiations and Arbitration

The Parties agree that the following Disputes are not subject to the
above provisions concerning informal negotiations and binding
arbitration:

- any Disputes seeking to enforce or protect, or concerning the
  validity of, any of the intellectual property rights of a Party;
- any Dispute related to, or arising from, allegations of theft,
  piracy, invasion of privacy, or unauthorized use; and
- any claim for injunctive relief.

If this provision is found to be illegal or unenforceable, then neither
Party will elect to arbitrate any Dispute falling within that portion of
this provision found to be illegal or unenforceable and such Dispute
shall be decided by a court of competent jurisdiction within the courts
listed for jurisdiction above, and the Parties agree to submit to the
personal jurisdiction of that court.

---

## 33. Miscellaneous

These Legal Terms and any policies or operating rules posted by us on
the Services or in respect to the Services constitute the entire
agreement and understanding between you and us. Our failure to exercise
or enforce any right or provision of these Legal Terms shall not operate
as a waiver of such right or provision. These Legal Terms operate to the
fullest extent permissible by law. We may assign any or all of our
rights and obligations to others at any time. We shall not be
responsible or liable for any loss, damage, delay, or failure to act
caused by any cause beyond our reasonable control. If any provision or
part of a provision of these Legal Terms is determined to be unlawful,
void, or unenforceable, that provision or part of the provision is
deemed severable from these Legal Terms and does not affect the
validity and enforceability of any remaining provisions. There is no
joint venture, partnership, employment, or agency relationship created
between you and us as a result of these Legal Terms or use of the
Services. You agree that these Legal Terms will not be construed against
us by virtue of having drafted them. You hereby waive any and all
defenses you may have based on the electronic form of these Legal Terms
and the lack of signing by the parties hereto to execute these Legal
Terms.

---

## 34. Contact Us

In order to resolve a complaint regarding the Services or to receive
further information regarding use of the Services, please contact us at:

> **Concord Voice LLC**
> 2008 Bremo Road, Suite 110
> Richmond, VA 23226
> United States
> Email:
> [contact-us@concordvoice.com](mailto:contact-us@concordvoice.com)

For privacy-specific inquiries:
[privacy@concordvoice.com](mailto:privacy@concordvoice.com)

For security-specific inquiries:
[security@concordvoice.com](mailto:security@concordvoice.com)

For DMCA notices:
[copyright-claims@concordvoice.com](mailto:copyright-claims@concordvoice.com)

For commercial-licensing inquiries (source code under CVSL):
[contact-us@concordvoice.com](mailto:contact-us@concordvoice.com) with
"Commercial License" in the subject line, or visit
[https://concordvoice.com/commercial-license](https://concordvoice.com/commercial-license).
