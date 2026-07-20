import { useEffect, useState } from 'react'
import { useTranslation } from 'react-i18next'
import { toast } from 'sonner'
import { Button } from '@/components/ui/button'
import { Textarea } from '@/components/ui/textarea'
import {
  getOptionValue,
  useSystemOptions,
} from '@/features/system-settings/hooks/use-system-options'
import { useUpdateOption } from '@/features/system-settings/hooks/use-update-option'

const RECHARGE_CONFIG_KEY = 'transwork_recharge.config'

const PLACEHOLDER = `{
  "credits_per_dollar": 100,
  "tiers": [
    { "usd": 10, "bonus_pct": 0 },
    { "usd": 30, "bonus_pct": 5 },
    { "usd": 100, "bonus_pct": 10 }
  ]
}`

// Gressio desktop recharge tiers + standard exchange rate. Edits the
// transwork_recharge.config option (a JSON blob); empty => the server's built-in
// default is used. The desktop client renders these tiers and the server grants
// the configured bonus credits, so the two always agree.
export function DesktopRechargeTiers() {
  const { t } = useTranslation()
  const { data } = useSystemOptions()
  const updateOption = useUpdateOption()
  const serverValue = getOptionValue(data?.data, {
    [RECHARGE_CONFIG_KEY]: '',
  })[RECHARGE_CONFIG_KEY] as string
  const [value, setValue] = useState('')

  useEffect(() => {
    setValue(serverValue || '')
  }, [serverValue])

  const handleSave = () => {
    const trimmed = value.trim()
    if (trimmed !== '') {
      try {
        JSON.parse(trimmed)
      } catch {
        toast.error(t('Recharge tiers config is not valid JSON'))
        return
      }
    }
    updateOption.mutate({ key: RECHARGE_CONFIG_KEY, value })
  }

  return (
    <div className='space-y-2 rounded-lg border p-4'>
      <div className='space-y-0.5'>
        <p className='text-base font-medium'>{t('Desktop recharge tiers')}</p>
        <p className='text-muted-foreground text-sm'>
          {t(
            'Recharge tiers and bonus for the Gressio desktop client. Leave empty to use the server default. credits_per_dollar is the standard rate (credits per $1); bonus_pct is extra credits granted on top.'
          )}
        </p>
      </div>
      <Textarea
        value={value}
        onChange={(e) => setValue(e.target.value)}
        rows={8}
        placeholder={PLACEHOLDER}
        className='font-mono text-xs'
      />
      <Button onClick={handleSave} disabled={updateOption.isPending}>
        {t('Save')}
      </Button>
    </div>
  )
}
