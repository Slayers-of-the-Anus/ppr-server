(() => {
  const form = document.getElementById('loginForm');
  const error = document.getElementById('error');
  form.addEventListener('submit', async e => {
    e.preventDefault(); error.textContent = '';
    const button = form.querySelector('button'); button.disabled = true;
    try {
      const res = await fetch('/api/login', { method:'POST', headers:{'content-type':'application/json'}, body:JSON.stringify({ username:form.username.value, password:form.password.value }) });
      if (!res.ok) throw new Error('Неверный логин или пароль');
      location.assign('/admin');
    } catch (err) { error.textContent = err.message; }
    finally { button.disabled = false; }
  });
})();
