(() => {
  const mismatchText = 'Los correos electrónicos no coinciden.';

  function currentForm() {
    return document.querySelector('#customer-content form');
  }

  function normalizeEmail(raw) {
    const value = raw.trim();
    const at = value.lastIndexOf('@');
    return value.slice(0, at + 1) + value.slice(at + 1).toLowerCase();
  }

  function reconcileEmails(edited) {
    const form = currentForm();
    if (!form) return;
    const email = form.elements.namedItem('email');
    const confirmation = form.elements.namedItem('email_confirm');
    const status = form.querySelector('#save-status-email_confirm');

    confirmation.setCustomValidity('');
    let syntaxError = status.querySelector('[data-confirm-error="syntax"]');
    if (edited === confirmation && syntaxError) {
      syntaxError.remove();
      syntaxError = null;
    }

    const mismatch = email.value.trim() !== '' && confirmation.value.trim() !== '' &&
      email.validity.valid && confirmation.validity.valid &&
      normalizeEmail(email.value) !== normalizeEmail(confirmation.value);
    const mismatchError = status.querySelector('[data-confirm-error="mismatch"]');
    if (mismatch) {
      confirmation.setCustomValidity(mismatchText);
      if (!syntaxError && !mismatchError) {
        const message = document.createElement('p');
        message.setAttribute('role', 'alert');
        message.dataset.confirmError = 'mismatch';
        message.textContent = mismatchText;
        status.append(message);
      }
    } else {
      mismatchError?.remove();
    }
    if (mismatch || syntaxError) {
      confirmation.setAttribute('aria-invalid', 'true');
    } else {
      confirmation.removeAttribute('aria-invalid');
    }
  }

  function isDraft(control) {
    return control?.matches('input[hx-params], select[hx-params]');
  }

  function activeDraft(control) {
    return isDraft(control) && control.isConnected && control.form === currentForm();
  }

  function ignoreDetachedDraft(event) {
    if (!activeDraft(event.currentTarget)) event.preventDefault();
  }

  document.addEventListener('input', (event) => {
    const control = event.target;
    if ((control.id === 'email' || control.id === 'email_confirm') && control.form === currentForm()) {
      reconcileEmails(control);
    }
  });

  document.addEventListener('htmx:beforeRequest', (event) => {
    const source = event.detail.requestConfig.elt;
    if (activeDraft(source)) {
      // Listen on the source itself: a detached draft cannot bubble to this document.
      source.addEventListener('htmx:beforeOnLoad', ignoreDetachedDraft);
    }
  });

  document.addEventListener('htmx:afterSwap', (event) => {
    const source = event.detail.requestConfig?.elt;
    if (isDraft(source)) {
      if (!activeDraft(source)) return;
      const status = document.getElementById(`save-status-${source.name}`);
      if (event.detail.elt !== status) return;
      const validation = status.querySelector('[data-validation]')?.dataset.validation;
      if (validation === 'invalid') source.setAttribute('aria-invalid', 'true');
      if (validation === 'valid') source.removeAttribute('aria-invalid');
      return;
    }
    if (!source?.matches('form[hx-target="#customer-content"]')) return;
    const form = currentForm();
    if (!form) return;
    reconcileEmails();
    if (event.detail.xhr.status === 422) {
      form.querySelector('[aria-invalid="true"], :invalid')?.focus();
    }
  });

  document.addEventListener('htmx:afterRequest', (event) => {
    if (event.detail.xhr.status !== 0) return;
    const source = event.detail.requestConfig?.elt;
    const form = currentForm();
    const target = activeDraft(source)
      ? document.getElementById(`save-status-${source.name}`)
      : source === form ? form.querySelector('#form-error') : null;
    if (!target) return;
    const message = document.createElement('p');
    message.setAttribute('role', 'alert');
    message.textContent = 'No se guardó. Verifica tu conexión e intenta de nuevo.';
    target.replaceChildren(message);
  });

  reconcileEmails();
})();
