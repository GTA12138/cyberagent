<?php
setcookie('portal_sid', '', ['expires' => time() - 3600, 'path' => '/']);
header('Location: /', true, 302);
