import type { Models } from '@/lib/api'

export function SitePrivacy({ url }: { url?: string }) {
  return url ? (
    <a className="underline" href={url} rel="noreferrer">
      Site privacy notice
    </a>
  ) : (
    <span>
      No site privacy notice is linked. Contact your operator for privacy
      information.
    </span>
  )
}

export function AIInteraction({ privacyURL }: { privacyURL?: string }) {
  return (
    <section className="panel p-6 mb-6" aria-labelledby="ai-interaction-title">
      <h2 id="ai-interaction-title" className="panel-title">
        Before you send a prompt
      </h2>
      <p className="subtitle">
        Your connected client interacts with an AI model. Responses may be
        inaccurate; check important information. The model backend receives the
        request your client sends, including prompts, conversation history, tool
        definitions and results, and any attachments included in that request.
      </p>
      <p className="subtitle">
        The router can send the full request to another permitted replica on a
        qualifying retry. Backend logging and retention follow the recipients’
        policies. Turning off personal request tracing does not stop this
        routing or backend processing.
      </p>
      <p className="subtitle">
        <SitePrivacy url={privacyURL} />
      </p>
    </section>
  )
}

export function ModelRecipients({
  model,
}: {
  model: Models['models'][number]
}) {
  return (
    <section className="p-6 border-t" aria-labelledby="recipients-title">
      <h3 id="recipients-title">Possible recipients for {model.id}</h3>
      <p className="subtitle">
        Any of these organizations may receive the request on initial selection
        or a qualifying retry. Temporarily unavailable destinations are
        included.
      </p>
      <ul className="list-disc pl-5 mt-3 space-y-2">
        {(model.recipients ?? []).map((recipient, index) => (
          <li key={index}>
            {recipient.organization || 'Unlabelled destination'}
            {' · '}
            {recipient.privacy_url ? (
              <a
                className="underline"
                href={recipient.privacy_url}
                rel="noreferrer"
              >
                Privacy notice for{' '}
                {recipient.organization || 'unlabelled destination'}
              </a>
            ) : (
              'No recipient privacy notice linked'
            )}
          </li>
        ))}
      </ul>
      {!model.recipients_complete && (
        <p className="notice mt-3">
          Recipient information is incomplete. At least one destination lacks a
          public organization label or privacy notice. Contact your operator
          before sending sensitive information.
        </p>
      )}
    </section>
  )
}
