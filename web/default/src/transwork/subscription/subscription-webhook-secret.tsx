import { useState } from 'react'
import { useTranslation } from 'react-i18next'
import { toast } from 'sonner'
import { Button } from '@/components/ui/button'
import { Input } from '@/components/ui/input'
import { Label } from '@/components/ui/label'
import {
  getOptionValue,
  useSystemOptions,
} from '@/features/system-settings/hooks/use-system-options'
import { useUpdateOption } from '@/features/system-settings/hooks/use-update-option'
import { removeTrailingSlash } from '@/features/system-settings/integrations/utils'

const WEBHOOK_SECRET_KEY = 'transwork_subscription.stripe_webhook_secret'

// The card title doubles as the field's <label>, so the input's accessible name
// distinguishes it from the identically-placeholdered top-up webhook secret above.
const FIELD_ID = 'transwork-subscription-webhook-secret'

const REQUIRED_EVENTS =
  'checkout.session.completed, invoice.payment_succeeded, ' +
  'invoice.payment_failed, customer.subscription.updated, ' +
  'customer.subscription.deleted'

// Signing secret for the recurring-subscription webhook ("endpoint B",
// transwork/handler/stripe_subscription_webhook.go), which is a SEPARATE Stripe
// endpoint from the upstream top-up one above and therefore has its own secret.
// The handler is fail-closed: while this is empty every delivery is rejected 503.
//
// Write-only by necessity, not by choice: controller.GetOptions drops any key
// ending in "secret", so the current value can never be read back and the field
// always renders empty. An empty save is refused rather than silently clearing a
// configured secret and taking the endpoint down.
export function SubscriptionWebhookSecret() {
  const { t } = useTranslation()
  const { data } = useSystemOptions()
  const updateOption = useUpdateOption()
  const [value, setValue] = useState('')

  const serverAddress = getOptionValue(data?.data, {
    ServerAddress: '',
  }).ServerAddress as string

  // Stripe needs an absolute HTTPS endpoint, so on an install that has not set
  // ServerAddress yet fall back to the same literal token the upstream webhook
  // rows use rather than rendering a bare, unusable relative path.
  const callbackUrl = `${
    removeTrailingSlash(serverAddress) || '<ServerAddress>'
  }/transwork/stripe/subscription-webhook`

  const handleSave = () => {
    const secret = value.trim()
    if (secret === '') {
      toast.error(t('Enter the subscription webhook signing secret'))
      return
    }
    updateOption.mutate(
      { key: WEBHOOK_SECRET_KEY, value: secret },
      // Clear only once the server confirms it stored the value. The API answers
      // a rejected save (e.g. a stale non-root session) with HTTP 200 and
      // success:false, which still resolves the mutation -- and because the
      // secret is write-only, clearing on that would lose what the admin typed
      // with no way to read it back.
      { onSuccess: (data) => data.success && setValue('') }
    )
  }

  return (
    <div className='space-y-2 rounded-lg border p-4'>
      <div className='space-y-0.5'>
        <Label htmlFor={FIELD_ID} className='text-base font-medium'>
          {t('Subscription webhook secret')}
        </Label>
        <p className='text-muted-foreground text-sm'>
          {t(
            'Subscription renewals use a separate Stripe webhook endpoint with its own signing secret, independent of the top-up webhook secret above. It is never echoed back, so the field is always blank; saving it empty is rejected.'
          )}
        </p>
        <p className='text-muted-foreground text-sm'>
          {t('Callback URL')}: {callbackUrl}
        </p>
        <p className='text-muted-foreground text-sm'>
          {t('Required events:')} {REQUIRED_EVENTS}
        </p>
      </div>
      <Input
        id={FIELD_ID}
        type='password'
        autoComplete='new-password'
        placeholder={t('whsec_xxx')}
        value={value}
        onChange={(event) => setValue(event.target.value)}
        // This card renders inside the payment form, so Enter would otherwise
        // implicitly submit that form -- saving the unrelated payment fields
        // while leaving this secret, which the form never reads, unsaved.
        onKeyDown={(event) => {
          if (event.key === 'Enter') {
            event.preventDefault()
            handleSave()
          }
        }}
      />
      {/* type='button' as every other button in the containing form does:
          without it the button defaults to submit and a click would save the
          whole payment form as a side effect. */}
      <Button
        type='button'
        onClick={handleSave}
        disabled={updateOption.isPending}
      >
        {t('Save')}
      </Button>
    </div>
  )
}
