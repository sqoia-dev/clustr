// login.js — handles the /login page form submission (ADR-0007).
//
// Sends username+password to POST /api/v1/auth/login.
// On force_password_change=true, redirects to /set-password.

(function () {
    const form      = document.getElementById('login-form');
    const usernameEl = document.getElementById('username');
    const passwordEl = document.getElementById('password');
    const btn       = document.getElementById('login-btn');
    const errEl     = document.getElementById('login-error');

    function showError(msg) {
        errEl.textContent = msg;
        errEl.classList.add('visible');
        usernameEl.focus();
    }

    function clearError() {
        errEl.classList.remove('visible');
        errEl.textContent = '';
    }

    form.addEventListener('submit', async function (e) {
        e.preventDefault();
        clearError();

        const username = (usernameEl.value || '').trim();
        const password = passwordEl.value || '';

        if (!username) {
            showError('Please enter your username.');
            return;
        }
        if (!password) {
            showError('Please enter your password.');
            return;
        }

        btn.disabled = true;
        btn.textContent = 'Signing in\u2026';

        try {
            const resp = await fetch('/api/v1/auth/login', {
                method: 'POST',
                headers: { 'Content-Type': 'application/json' },
                body: JSON.stringify({ username, password }),
                credentials: 'same-origin',
            });

            if (resp.ok) {
                let body = {};
                try { body = await resp.json(); } catch (_) {}

                // Clear any legacy localStorage key from the old modal flow.
                try { localStorage.removeItem('clonr_admin_key'); } catch (_) {}

                if (body.force_password_change) {
                    window.location.href = '/set-password';
                } else {
                    window.location.href = '/';
                }
                return;
            }

            let msg = 'Invalid username or password.';
            try {
                const body = await resp.json();
                if (body && body.error) msg = body.error;
            } catch (_) {}
            showError(msg);
        } catch (err) {
            showError('Network error \u2014 could not reach the server.');
        } finally {
            btn.disabled = false;
            btn.textContent = 'Sign in';
        }
    });
}());
