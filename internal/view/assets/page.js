(function() {
  const data = JSON.parse(document.getElementById('data').textContent);

  const header = document.createElement('header');
  const title = document.createElement('h1');
  title.textContent = 'witness';
  header.appendChild(title);

  const nav = document.createElement('nav');
  const overviewBtn = document.createElement('button');
  overviewBtn.textContent = 'Overview';
  overviewBtn.setAttribute('data-page', 'overview');
  const changedBtn = document.createElement('button');
  changedBtn.textContent = 'How you\'ve changed';
  changedBtn.setAttribute('data-page', 'changed');

  nav.appendChild(overviewBtn);
  nav.appendChild(changedBtn);
  header.appendChild(nav);

  const overviewSection = document.createElement('section');
  overviewSection.setAttribute('data-page', 'overview');

  const changedSection = document.createElement('section');
  changedSection.setAttribute('data-page', 'changed');
  changedSection.style.display = 'none';

  let currentPage = 'overview';

  function switchPage(page) {
    currentPage = page;
    overviewSection.style.display = page === 'overview' ? 'block' : 'none';
    changedSection.style.display = page === 'changed' ? 'block' : 'none';
  }

  overviewBtn.addEventListener('click', function() { switchPage('overview'); });
  changedBtn.addEventListener('click', function() { switchPage('changed'); });

  document.body.appendChild(header);
  document.body.appendChild(overviewSection);
  document.body.appendChild(changedSection);
})();
