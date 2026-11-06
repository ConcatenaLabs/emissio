// "Connect your web wallet" on the link task, through the connector layer SeqDEX uses
// (static/shared, vendored from seqdex-web). The wallet signs "Emissio <account code>" behind its
// own approval (the provider protocol's signMessage); the address and signature then go in the
// task's notes and are submitted as a pasted pair would be. The server checks the signature itself
// and trusts nothing this page says.
//
// The connectors offered are the server's setting (EMISSIO_CONNECTORS), in data-connectors: one
// is connected at the press of the button, several are listed to choose from.
const box = document.querySelector('[data-connect-wallet]')
const form = document.querySelector('form[action$="/submit"]')
if (box && form) start()

async function start () {
  const offer = (box.dataset.connectors || 'webwallet').split(',').filter(Boolean)
  globalThis.SEQ_PROVIDER_SITE = { name: 'Emissio', choiceKey: 'emissio.connector', offer }
  const P = await import('./shared/provider.js')
  const btn = box.querySelector('[data-connect]')
  const list = box.querySelector('[data-choices]')
  const line = box.querySelector('[data-line]')
  const say = (t) => { line.textContent = t }
  box.hidden = false

  async function sign (c) {
    await P.choose(c.id)
    if (c.id === 'webwallet' && !(await c.available())) {
      c.open()
      return say('Your web wallet is opening in a new tab. Unlock it there, then press Connect again.')
    }
    if (c.id === 'phone' && !c.paired()) await pairPhone(c)
    if (!(await P.supports('signMessage'))) return say('This wallet cannot sign the message yet. Sign it by hand below.')
    say('Approve in your wallet.')
    let a
    try {
      a = await P.request('signMessage', { message: box.dataset.message })
    } catch (e) {
      const m = (e && e.message) || String(e)
      if (/rejected/.test(m)) return say('You declined in the wallet. Nothing was linked.')
      if (/locked/.test(m)) return say('Unlock your wallet, then press Connect again.')
      if (/not open|not installed|not answering|not paired|did not answer/.test(m)) return say('Your wallet did not answer. Open it, then press Connect again, or sign the message by hand below.')
      return say('Your wallet could not sign: ' + m)
    }
    form.querySelector('[name=notes]').value = a.address + '\n' + a.signature
    say('Signed. Linking your wallet…')
    form.submit()
  }

  // A wallet on a phone pairs first: a code to scan, or a link on the same phone.
  async function pairPhone (c) {
    const { uri, paired } = await c.pair()
    const { default: qrcode } = await import('./shared/vendor/qrcode.mjs')
    const q = qrcode(0, 'L'); q.addData(uri); q.make()
    const pair = box.querySelector('[data-pair]')
    pair.replaceChildren()
    const img = document.createElement('img')
    img.src = q.createDataURL(4, 8); img.alt = 'Pairing code'
    const a = document.createElement('a')
    a.href = uri; a.textContent = 'Open in the wallet on this phone'
    pair.append(img, a)
    pair.hidden = false
    say('Scan the code with your wallet. Waiting for your wallet…')
    await paired
    pair.hidden = true
  }

  const run = (c) => async () => {
    btn.disabled = true
    try { await sign(c) } catch (e) { say('Your wallet could not sign: ' + ((e && e.message) || e)) } finally { btn.disabled = false }
  }
  const conns = P.connectors()
  if (conns.length === 1) {
    btn.addEventListener('click', run(conns[0]))
    return
  }
  btn.addEventListener('click', () => { list.hidden = !list.hidden })
  for (const c of conns) {
    const b = document.createElement('button')
    b.type = 'button'; b.className = 'btn ghost small'; b.textContent = c.label
    b.addEventListener('click', run(c))
    list.append(b)
  }
}
