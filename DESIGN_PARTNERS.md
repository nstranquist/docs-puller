# Hosted Team founding design partner

docs-puller will always be a local-first open-source CLI first. The proprietary
hosted Team service is for teams that want the same private-corpus retrieval
without operating synchronization, tenancy, access control, and evaluation.

I am recruiting **one founding design partner** for a paid, single-tenant pilot.
This is a concierge engagement, not a claim that the general-purpose hosted
service is publicly launched.

## Offer

- **Price:** $1,500 fixed for 30 days; no automatic renewal.
- **Team:** up to 10 users.
- **Corpus:** up to 3 private repositories or documentation spaces plus up to
  10 vendor sources supported by the existing pull surface.
- **Deployment:** isolated managed instance, or a customer-controlled account
  when the security review requires it.
- **Retrieval:** local/BM25 retrieval by default; optional embedding and LLM
  reranking is BYOK and activated only with explicit data-boundary approval.
- **Onboarding:** source inventory, access-boundary review, initial ingest,
  scheduled refresh, and a customer-specific fixture of at least 25 important
  queries.
- **Operation:** weekly health and retrieval-quality review, one shared support
  channel, and reasonable connector fixes inside the agreed source set.
- **Exit:** Markdown corpus export, eval fixture and results, configuration
  export, and hosted-data deletion within 7 days of written confirmation.

The provisional post-pilot Team price is **$199/month for up to 10 users**, plus
provider usage at cost when optional hosted model calls are enabled. That price
is modeled and will not be represented as validated until a partner agrees to
continue.

## Good fit

- A 5–30 person engineering team already using coding agents or AI-enabled IDEs.
- Important knowledge is split across private repositories, internal docs, and
  fast-changing vendor documentation.
- A hosted third-party retrieval service is unacceptable or insufficiently
  measurable, but the team does not want to operate its own ingestion plane.
- One technical owner can provide access, validate 25+ real queries, and join a
  weekly 30-minute review.

## Success contract

The kickoff records a baseline and target rather than promising a generic
quality number. A successful pilot must demonstrate all of the following:

1. agreed sources synchronize through the isolated tenant without cross-tenant
   access;
2. at least 25 customer-authored queries have human-validated expected paths;
3. retrieval meets the agreed Hit@5 target or improves at least 15 percentage
   points over the kickoff baseline;
4. at least 3 team members complete 50 provenance-labeled real searches;
5. the operator receives a health, freshness, cost, and retrieval report; and
6. the team chooses to continue, export and self-host, or delete without lock-in.

## Not included

- public multi-tenant availability, enterprise SLA, or multi-region failover;
- SAML/SCIM, custom legal or compliance certification, or regulated data unless
  separately reviewed;
- unlimited repositories, arbitrary new connectors, or data migration outside
  the stated export;
- provider keys supplied by the operator without an approved BYOK boundary; or
- a claim that payment alone validates the future $199/month plan.

## Apply

Open a
[design-partner intake issue](https://github.com/nstranquist/docs-puller/issues/new?template=design-partner.yml).
The issue is public: describe the workflow and source categories, but do not
include repository names, credentials, private URLs, or confidential content.
Detailed access and security discussion happens privately after fit is
confirmed.
